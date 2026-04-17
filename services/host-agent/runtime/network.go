package runtime

// network.go — TAP device creation/deletion and iptables/ip6tables NAT rule programming.
//
// Source: RUNTIMESERVICE_GRPC_V1 §7 steps 2-4,
//         07-01-phase-1-network-architecture-and-ip-model.md,
//         IMPLEMENTATION_PLAN_V1 §31, §34.
//
// TAP device lifecycle:
//   CreateTAP(instanceID, macAddr) → tap device name (e.g. tap-<8 chars of instanceID>)
//   DeleteTAP(instanceID)          → idempotent; no-op if device absent
//
// IPv4 NAT rules (public IP only — Phase 1 NAT):
//   ProgramNAT(instanceID, privateIP, publicIP)  → DNAT inbound, SNAT outbound
//   RemoveNAT(instanceID, privateIP, publicIP)   → idempotent removal
//
// IPv6 NAT rules (VM-P3A Job 1 — dual-stack foundation):
//   ProgramNATv6(instanceID, privateIPv6, publicIPv6)  → ip6tables DNAT+SNAT
//   RemoveNATv6(instanceID, privateIPv6, publicIPv6)   → idempotent removal
//
// Dual-stack convenience wrappers:
//   ProgramDualStackNAT(instanceID, privateIP, publicIP, privateIPv6, publicIPv6)
//   RemoveDualStackNAT(instanceID, privateIP, publicIP, privateIPv6, publicIPv6)
//
// All operations are idempotent. Callers may retry on transient failures.
//
// Requires: ip(8), iptables(8), ip6tables(8) on PATH.
// Must run as root (or with CAP_NET_ADMIN).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// NetworkManager handles TAP device and iptables lifecycle.
type NetworkManager struct {
	dryRun bool // NETWORK_DRY_RUN=true: skip ip(8)/iptables(8) calls for local dev
	log    *slog.Logger
}

// NewNetworkManager constructs a NetworkManager.
// Set NETWORK_DRY_RUN=true on macOS / non-Linux hosts where ip(8) and
// iptables(8) are not available. In dry-run mode all operations log at WARN
// and return nil — no actual kernel network changes are made.
func NewNetworkManager(log *slog.Logger) *NetworkManager {
	return &NetworkManager{
		dryRun: os.Getenv("NETWORK_DRY_RUN") == "true",
		log:    log,
	}
}

// tapName returns the deterministic TAP device name for an instance.
// Format: tap-<first 8 chars of instanceID> — short enough for Linux (max 15 chars).
func tapName(instanceID string) string {
	suffix := instanceID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return "tap-" + suffix
}

// CreateTAP creates a TAP network device and brings it up.
// Idempotent: if the device already exists (from a prior attempt), succeeds without error.
// Returns the tap device name for use in the Firecracker config.
//
// Source: IMPLEMENTATION_PLAN_V1 §31 (implement TAP device create/delete).
func (n *NetworkManager) CreateTAP(ctx context.Context, instanceID, macAddr string) (string, error) {
	dev := tapName(instanceID)

	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping TAP creation — no kernel network changes made",
			"instance_id", instanceID,
			"tap_device", dev,
		)
		return dev, nil
	}

	// Check if device already exists (idempotent).
	checkCmd := exec.CommandContext(ctx, "ip", "link", "show", dev)
	if err := checkCmd.Run(); err == nil {
		n.log.Info("TAP device already exists — reusing",
			"instance_id", instanceID,
			"tap_device", dev,
		)
		return dev, nil
	}

	// ip tuntap add dev <tap> mode tap
	if err := n.run(ctx, "ip", "tuntap", "add", "dev", dev, "mode", "tap"); err != nil {
		return "", fmt.Errorf("CreateTAP: add tuntap: %w", err)
	}

	// Set MAC address if provided.
	if macAddr != "" {
		if err := n.run(ctx, "ip", "link", "set", dev, "address", macAddr); err != nil {
			// Best-effort: log warning but don't fail (Firecracker sets its own MAC).
			n.log.Warn("CreateTAP: could not set MAC address",
				"tap_device", dev,
				"mac", macAddr,
				"error", err,
			)
		}
	}

	// ip link set <tap> up
	if err := n.run(ctx, "ip", "link", "set", dev, "up"); err != nil {
		// Clean up the created device before returning error.
		_ = n.run(ctx, "ip", "link", "delete", dev)
		return "", fmt.Errorf("CreateTAP: set up: %w", err)
	}

	n.log.Info("TAP device created and up",
		"instance_id", instanceID,
		"tap_device", dev,
		"mac", macAddr,
	)
	return dev, nil
}

// DeleteTAP removes the TAP device for an instance.
// Idempotent: if the device does not exist, returns nil.
//
// Source: IMPLEMENTATION_PLAN_V1 §31.
func (n *NetworkManager) DeleteTAP(ctx context.Context, instanceID string) error {
	dev := tapName(instanceID)

	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping TAP deletion",
			"instance_id", instanceID,
			"tap_device", dev,
		)
		return nil
	}

	// Check if the device exists before trying to delete it.
	checkCmd := exec.CommandContext(ctx, "ip", "link", "show", dev)
	if err := checkCmd.Run(); err != nil {
		n.log.Info("TAP device already absent — idempotent no-op",
			"instance_id", instanceID,
			"tap_device", dev,
		)
		return nil
	}

	if err := n.run(ctx, "ip", "link", "delete", dev); err != nil {
		return fmt.Errorf("DeleteTAP: %w", err)
	}
	n.log.Info("TAP device deleted", "instance_id", instanceID, "tap_device", dev)
	return nil
}

// ProgramNAT installs iptables DNAT (inbound) and SNAT (outbound) rules
// to route traffic between the public IP and the instance's private IP.
//
// Idempotent: uses -C (check) before -A (append) to avoid duplicate rules.
// publicIP may be empty — in that case, no rules are installed.
//
// Source: IMPLEMENTATION_PLAN_V1 §34 (iptables DNAT/SNAT rule programming).
func (n *NetworkManager) ProgramNAT(ctx context.Context, instanceID, privateIP, publicIP string) error {
	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping NAT programming — no iptables changes made",
			"instance_id", instanceID,
			"private_ip", privateIP,
			"public_ip", publicIP,
		)
		return nil
	}
	if publicIP == "" {
		n.log.Info("no public IP — skipping NAT rules", "instance_id", instanceID)
		return nil
	}

	// PREROUTING DNAT: incoming traffic to publicIP → privateIP
	dnatArgs := []string{
		"-t", "nat", "-A", "PREROUTING",
		"-d", publicIP,
		"-j", "DNAT", "--to-destination", privateIP,
		"-m", "comment", "--comment", "cpvm-" + instanceID,
	}
	if err := n.iptablesIdempotent(ctx, dnatArgs); err != nil {
		return fmt.Errorf("ProgramNAT: DNAT: %w", err)
	}

	// POSTROUTING SNAT: outgoing traffic from privateIP → publicIP
	snatArgs := []string{
		"-t", "nat", "-A", "POSTROUTING",
		"-s", privateIP,
		"-j", "SNAT", "--to-source", publicIP,
		"-m", "comment", "--comment", "cpvm-" + instanceID,
	}
	if err := n.iptablesIdempotent(ctx, snatArgs); err != nil {
		return fmt.Errorf("ProgramNAT: SNAT: %w", err)
	}

	n.log.Info("NAT rules programmed",
		"instance_id", instanceID,
		"private_ip", privateIP,
		"public_ip", publicIP,
	)
	return nil
}

// RemoveNAT deletes iptables DNAT and SNAT rules for the instance.
// Idempotent: if the rules are absent, returns nil.
// publicIP may be empty — in that case, returns nil immediately.
//
// Source: IMPLEMENTATION_PLAN_V1 §34.
func (n *NetworkManager) RemoveNAT(ctx context.Context, instanceID, privateIP, publicIP string) error {
	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping NAT removal",
			"instance_id", instanceID,
			"private_ip", privateIP,
			"public_ip", publicIP,
		)
		return nil
	}
	if publicIP == "" {
		return nil
	}

	// Delete DNAT rule (replace -A with -D for delete).
	dnatArgs := []string{
		"-t", "nat", "-D", "PREROUTING",
		"-d", publicIP,
		"-j", "DNAT", "--to-destination", privateIP,
		"-m", "comment", "--comment", "cpvm-" + instanceID,
	}
	if err := n.iptablesDeleteIdempotent(ctx, dnatArgs); err != nil {
		return fmt.Errorf("RemoveNAT: DNAT: %w", err)
	}

	// Delete SNAT rule.
	snatArgs := []string{
		"-t", "nat", "-D", "POSTROUTING",
		"-s", privateIP,
		"-j", "SNAT", "--to-source", publicIP,
		"-m", "comment", "--comment", "cpvm-" + instanceID,
	}
	if err := n.iptablesDeleteIdempotent(ctx, snatArgs); err != nil {
		return fmt.Errorf("RemoveNAT: SNAT: %w", err)
	}

	n.log.Info("NAT rules removed",
		"instance_id", instanceID,
		"private_ip", privateIP,
		"public_ip", publicIP,
	)
	return nil
}

// ── IPv6 NAT rules — VM-P3A Job 1 ────────────────────────────────────────────
//
// IPv6 NAT uses ip6tables with the same PREROUTING DNAT + POSTROUTING SNAT
// pattern as the IPv4 path. Requires net.ipv6.conf.all.forwarding=1 on the host.
//
// Source: vm-14-03__blueprint__ §future_phases "IPv6 Integration"
//   (Egress-Only Internet Gateways for IPv6).
// Note: full Egress-Only IGW semantics are a later job; these methods establish
// the host-agent seam so callers can wire IPv6 NAT without future surgery here.

// ProgramNATv6 installs ip6tables DNAT (inbound) and SNAT (outbound) rules
// to route traffic between a public IPv6 address and the instance's private IPv6 address.
//
// Mirrors ProgramNAT exactly, using ip6tables instead of iptables.
// Idempotent: uses -C (check) before -A (append) to avoid duplicate rules.
// publicIPv6 may be empty — in that case, no rules are installed.
//
// Source: vm-14-03__blueprint__ §future_phases "IPv6 Integration".
func (n *NetworkManager) ProgramNATv6(ctx context.Context, instanceID, privateIPv6, publicIPv6 string) error {
	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping IPv6 NAT programming — no ip6tables changes made",
			"instance_id", instanceID,
			"private_ipv6", privateIPv6,
			"public_ipv6", publicIPv6,
		)
		return nil
	}
	if publicIPv6 == "" {
		n.log.Info("no public IPv6 address — skipping IPv6 NAT rules", "instance_id", instanceID)
		return nil
	}

	// PREROUTING DNAT: incoming IPv6 traffic to publicIPv6 → privateIPv6
	dnatArgs := []string{
		"-t", "nat", "-A", "PREROUTING",
		"-d", publicIPv6,
		"-j", "DNAT", "--to-destination", privateIPv6,
		"-m", "comment", "--comment", "cpvm6-" + instanceID,
	}
	if err := n.ip6tablesIdempotent(ctx, dnatArgs); err != nil {
		return fmt.Errorf("ProgramNATv6: DNAT: %w", err)
	}

	// POSTROUTING SNAT: outgoing IPv6 traffic from privateIPv6 → publicIPv6
	snatArgs := []string{
		"-t", "nat", "-A", "POSTROUTING",
		"-s", privateIPv6,
		"-j", "SNAT", "--to-source", publicIPv6,
		"-m", "comment", "--comment", "cpvm6-" + instanceID,
	}
	if err := n.ip6tablesIdempotent(ctx, snatArgs); err != nil {
		return fmt.Errorf("ProgramNATv6: SNAT: %w", err)
	}

	n.log.Info("IPv6 NAT rules programmed",
		"instance_id", instanceID,
		"private_ipv6", privateIPv6,
		"public_ipv6", publicIPv6,
	)
	return nil
}

// RemoveNATv6 deletes ip6tables DNAT and SNAT rules for the instance.
// Idempotent: if the rules are absent, returns nil.
// publicIPv6 may be empty — in that case, returns nil immediately.
//
// Source: vm-14-03__blueprint__ §future_phases "IPv6 Integration".
func (n *NetworkManager) RemoveNATv6(ctx context.Context, instanceID, privateIPv6, publicIPv6 string) error {
	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping IPv6 NAT removal",
			"instance_id", instanceID,
			"private_ipv6", privateIPv6,
			"public_ipv6", publicIPv6,
		)
		return nil
	}
	if publicIPv6 == "" {
		return nil
	}

	// Delete DNAT rule.
	dnatArgs := []string{
		"-t", "nat", "-D", "PREROUTING",
		"-d", publicIPv6,
		"-j", "DNAT", "--to-destination", privateIPv6,
		"-m", "comment", "--comment", "cpvm6-" + instanceID,
	}
	if err := n.ip6tablesDeleteIdempotent(ctx, dnatArgs); err != nil {
		return fmt.Errorf("RemoveNATv6: DNAT: %w", err)
	}

	// Delete SNAT rule.
	snatArgs := []string{
		"-t", "nat", "-D", "POSTROUTING",
		"-s", privateIPv6,
		"-j", "SNAT", "--to-source", publicIPv6,
		"-m", "comment", "--comment", "cpvm6-" + instanceID,
	}
	if err := n.ip6tablesDeleteIdempotent(ctx, snatArgs); err != nil {
		return fmt.Errorf("RemoveNATv6: SNAT: %w", err)
	}

	n.log.Info("IPv6 NAT rules removed",
		"instance_id", instanceID,
		"private_ipv6", privateIPv6,
		"public_ipv6", publicIPv6,
	)
	return nil
}

// ProgramDualStackNAT programs both IPv4 and IPv6 NAT rules for a dual-stack instance.
// Either IP version may be empty — only the non-empty ones are programmed.
// This is a convenience wrapper; callers may also call ProgramNAT and ProgramNATv6 directly.
//
// Source: vm-14-01__blueprint__ §core_contracts "Dual-Stack Mandate".
func (n *NetworkManager) ProgramDualStackNAT(
	ctx context.Context,
	instanceID, privateIP, publicIP, privateIPv6, publicIPv6 string,
) error {
	if err := n.ProgramNAT(ctx, instanceID, privateIP, publicIP); err != nil {
		return fmt.Errorf("ProgramDualStackNAT: ipv4: %w", err)
	}
	if err := n.ProgramNATv6(ctx, instanceID, privateIPv6, publicIPv6); err != nil {
		return fmt.Errorf("ProgramDualStackNAT: ipv6: %w", err)
	}
	return nil
}

// RemoveDualStackNAT removes both IPv4 and IPv6 NAT rules for a dual-stack instance.
// Either IP version may be empty — only the non-empty ones are removed.
// Idempotent: safe to call on stop and delete paths.
//
// Source: vm-14-01__blueprint__ §core_contracts "Dual-Stack Mandate".
func (n *NetworkManager) RemoveDualStackNAT(
	ctx context.Context,
	instanceID, privateIP, publicIP, privateIPv6, publicIPv6 string,
) error {
	if err := n.RemoveNAT(ctx, instanceID, privateIP, publicIP); err != nil {
		return fmt.Errorf("RemoveDualStackNAT: ipv4: %w", err)
	}
	if err := n.RemoveNATv6(ctx, instanceID, privateIPv6, publicIPv6); err != nil {
		return fmt.Errorf("RemoveDualStackNAT: ipv6: %w", err)
	}
	return nil
}

// ── iptables helpers (IPv4) ───────────────────────────────────────────────────

// iptablesIdempotent checks if an iptables rule exists before appending it.
// checkArgs must be the -A version; internally converts to -C for the check.
func (n *NetworkManager) iptablesIdempotent(ctx context.Context, appendArgs []string) error {
	// Build -C (check) version by replacing -A with -C at position 2.
	checkArgs := make([]string, len(appendArgs))
	copy(checkArgs, appendArgs)
	for i, a := range checkArgs {
		if a == "-A" {
			checkArgs[i] = "-C"
			break
		}
	}
	checkCmd := exec.CommandContext(ctx, "iptables", checkArgs...)
	if err := checkCmd.Run(); err == nil {
		// Rule already exists.
		return nil
	}
	return n.run(ctx, "iptables", appendArgs...)
}

// iptablesDeleteIdempotent deletes an iptables rule; returns nil if already absent.
func (n *NetworkManager) iptablesDeleteIdempotent(ctx context.Context, deleteArgs []string) error {
	// Use -C to check existence first.
	checkArgs := make([]string, len(deleteArgs))
	copy(checkArgs, deleteArgs)
	for i, a := range checkArgs {
		if a == "-D" {
			checkArgs[i] = "-C"
			break
		}
	}
	checkCmd := exec.CommandContext(ctx, "iptables", checkArgs...)
	if err := checkCmd.Run(); err != nil {
		// Rule does not exist — idempotent no-op.
		return nil
	}
	return n.run(ctx, "iptables", deleteArgs...)
}

// ── ip6tables helpers (IPv6) — VM-P3A Job 1 ──────────────────────────────────
//
// Exact mirrors of the iptables helpers, using ip6tables instead.
// The -C / -A / -D flag logic is identical.

// ip6tablesIdempotent checks if an ip6tables rule exists before appending it.
func (n *NetworkManager) ip6tablesIdempotent(ctx context.Context, appendArgs []string) error {
	checkArgs := make([]string, len(appendArgs))
	copy(checkArgs, appendArgs)
	for i, a := range checkArgs {
		if a == "-A" {
			checkArgs[i] = "-C"
			break
		}
	}
	checkCmd := exec.CommandContext(ctx, "ip6tables", checkArgs...)
	if err := checkCmd.Run(); err == nil {
		// Rule already exists.
		return nil
	}
	return n.run(ctx, "ip6tables", appendArgs...)
}

// ip6tablesDeleteIdempotent deletes an ip6tables rule; returns nil if already absent.
func (n *NetworkManager) ip6tablesDeleteIdempotent(ctx context.Context, deleteArgs []string) error {
	checkArgs := make([]string, len(deleteArgs))
	copy(checkArgs, deleteArgs)
	for i, a := range checkArgs {
		if a == "-D" {
			checkArgs[i] = "-C"
			break
		}
	}
	checkCmd := exec.CommandContext(ctx, "ip6tables", checkArgs...)
	if err := checkCmd.Run(); err != nil {
		// Rule does not exist — idempotent no-op.
		return nil
	}
	return n.run(ctx, "ip6tables", deleteArgs...)
}

// run executes a shell command and returns an error with combined output on failure.
func (n *NetworkManager) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cmd %s %s: %w\noutput: %s",
			name, strings.Join(args, " "), err, string(out))
	}
	return nil
}

// ── VM-P2A-S3: Security Group Policy Enforcement Seam ────────────────────────
//
// Ownership boundary:
//   - resource-manager (API): SG object CRUD and rule admission validation.
//   - host-agent (here):       enforcement of SG rules at the hypervisor data plane.
//   - network-controller:      IP allocation/release only; does not own policy.
//
// Current implementation: stubs that log intent but perform no data-plane changes.
// Full nftables/conntrack programming is deferred to a later VM networking phase.
// The seam is established now so ownership is explicit and future work can replace
// these stubs without touching other layers.
//
// Source: vm-14-02 skill §instructions (\"Deploy a Host Enforcement Agent on each
// hypervisor to subscribe to policy updates, translate them into vSwitch rules\").

// SGRule represents a single security group rule passed to the host-agent enforcement seam.
// Mirrors the API/DB rule shape; defined here to avoid cross-package import from host-agent into db.
type SGRule struct {
	ID        string
	Direction string  // "ingress" | "egress"
	Protocol  string  // "tcp" | "udp" | "icmp" | "all"
	PortFrom  *int    // nil = any port
	PortTo    *int    // nil = any port
	CIDR      *string // nil when source is a security group reference
}

// ApplySGPolicy programs security group rules for the given instance TAP interface.
//
// In the full implementation this translates the SGRule slice into nftables rules
// anchored to tapDevice. The implicit deny-all default is installed first; rules
// from all attached SGs are union-merged.
//
// VM-P2A-S3: stub — logs intent, returns nil. Safe to call from create/start paths.
func (n *NetworkManager) ApplySGPolicy(_ context.Context, instanceID, tapDevice string, rules []SGRule) error {
	if n.dryRun || len(rules) == 0 {
		n.log.Info("ApplySGPolicy: no-op",
			"instance_id", instanceID,
			"tap_device", tapDevice,
			"rule_count", len(rules),
			"dry_run", n.dryRun,
		)
		return nil
	}
	// STUB: log policy intent; full nftables programming deferred.
	// When implemented:
	//   1. Flush existing per-NIC nftables chain for instanceID.
	//   2. Install implicit deny-all as base chain policy.
	//   3. Add allow matches per direction/protocol/port/cidr for each rule.
	n.log.Warn("ApplySGPolicy: SG policy not yet enforced at data plane — stub only",
		"instance_id", instanceID,
		"tap_device", tapDevice,
		"rule_count", len(rules),
	)
	return nil
}

// RemoveSGPolicy tears down security group enforcement state for a given instance NIC.
// Must be called on stop and delete paths to prevent stale nftables rules granting
// access to new VMs reusing the same TAP device or IP.
//
// VM-P2A-S3: stub — always returns nil. Idempotent by design.
//
// Source: vm-14-02 skill §failure_modes ("Stale IP rules … can grant unintended
// access to new VMs").
func (n *NetworkManager) RemoveSGPolicy(_ context.Context, instanceID, tapDevice string) error {
	n.log.Info("RemoveSGPolicy: no-op",
		"instance_id", instanceID,
		"tap_device", tapDevice,
		"dry_run", n.dryRun,
	)
	// STUB: when implemented, flushes the per-NIC nftables chain.
	return nil
}
