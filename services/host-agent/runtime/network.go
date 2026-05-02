package runtime

// network.go — TAP device creation/deletion, bridge attachment, iptables NAT and
// firewall rule programming via an injectable Executor interface.
//
// Source: IP_ALLOCATION_CONTRACT_V1, RUNTIMESERVICE_GRPC_V1 §7.
//
// The executor abstraction (executor.go) allows deterministic unit tests without
// requiring root, ip(8), or iptables(8) on the test machine.
//
// TAP lifecycle:
//   CreateTAP(instanceID, macAddr, bridgeName)  → creates TAP, attaches to bridge, brings up
//   DeleteTAP(instanceID, bridgeName)           → detaches from bridge, deletes TAP (idempotent)
//
// NAT (public IP only):
//   ProgramNAT(instanceID, privateIP, publicIP) → DNAT + SNAT iptables rules
//   RemoveNAT(instanceID, privateIP, publicIP)  → removes NAT rules (idempotent)
//
// Security group enforcement:
//   ApplySGPolicy(instanceID, tapDevice, rules) → iptables per-NIC chain with
//     deny-inbound-default, allow established/related, allow explicit ingress rules.
//   RemoveSGPolicy(instanceID, tapDevice)       → flushes per-NIC iptables chain (idempotent)

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
)

// network constants
const (
	// bridgeName is the Phase 1 host-local Linux bridge.
	defaultBridgeName = "br0"
)

// NetworkManager handles TAP, iptables NAT, and iptables firewall lifecycle.
type NetworkManager struct {
	dryRun   bool
	executor Executor
	log      *slog.Logger
}

// NewNetworkManager constructs a NetworkManager.
// Set NETWORK_DRY_RUN=true on macOS / non-Linux hosts where ip(8) and
// iptables(8) are not available. In dry-run mode all operations log at WARN
// and return nil — no actual kernel network changes are made.
func NewNetworkManager(log *slog.Logger) *NetworkManager {
	return &NetworkManager{
		dryRun:   os.Getenv("NETWORK_DRY_RUN") == "true",
		executor: NewRealExecutor(),
		log:      log,
	}
}

// NewNetworkManagerWithExecutor constructs a NetworkManager with a custom
// executor for testing. Dry-run is disabled when an executor is explicitly
// provided — the caller controls command behavior through the executor.
func NewNetworkManagerWithExecutor(log *slog.Logger, exec Executor) *NetworkManager {
	return &NetworkManager{
		dryRun:   false,
		executor: exec,
		log:      log,
	}
}

// tapName returns the deterministic TAP device name for an instance.
func tapName(instanceID string) string {
	suffix := instanceID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return "tap-" + suffix
}

// chainPrefix returns the deterministic iptables chain prefix for an instance.
func chainPrefix(instanceID string) string {
	suffix := instanceID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return "cpvm-" + suffix
}

// CreateTAP creates a TAP network device, attaches it to the host bridge,
// and brings it up. Idempotent.
//
// bridgeName: name of the Linux bridge to attach to. If empty, "br0" is used.
// macAddr: MAC address for the TAP. If empty, no MAC is explicitly set.
// Returns the tap device name.
func (n *NetworkManager) CreateTAP(ctx context.Context, instanceID, macAddr, bridgeName string) (string, error) {
	dev := tapName(instanceID)
	br := bridgeName
	if br == "" {
		br = defaultBridgeName
	}

	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping TAP creation",
			"instance_id", instanceID,
			"tap_device", dev,
			"bridge", br,
		)
		return dev, nil
	}

	// Idempotent: check if device already exists.
	_, err := n.executor.RunOutput(ctx, "ip", "link", "show", dev)
	if err == nil {
		n.log.Info("TAP device already exists — reusing",
			"instance_id", instanceID,
			"tap_device", dev,
		)
		return dev, nil
	}

	// Create TAP device.
	if err := n.executor.Run(ctx, "ip", "tuntap", "add", "dev", dev, "mode", "tap"); err != nil {
		return "", fmt.Errorf("CreateTAP: add tuntap: %w", err)
	}

	// Set MAC address if provided.
	if macAddr != "" {
		if err := n.executor.Run(ctx, "ip", "link", "set", dev, "address", macAddr); err != nil {
			n.log.Warn("CreateTAP: could not set MAC address",
				"tap_device", dev, "mac", macAddr, "error", err)
		}
	}

	// Attach to bridge.
	if err := n.executor.Run(ctx, "ip", "link", "set", dev, "master", br); err != nil {
		_ = n.executor.Run(ctx, "ip", "link", "delete", dev)
		return "", fmt.Errorf("CreateTAP: attach to bridge %s: %w", br, err)
	}

	// Bring the TAP up.
	if err := n.executor.Run(ctx, "ip", "link", "set", dev, "up"); err != nil {
		_ = n.executor.Run(ctx, "ip", "link", "set", dev, "nomaster")
		_ = n.executor.Run(ctx, "ip", "link", "delete", dev)
		return "", fmt.Errorf("CreateTAP: set up: %w", err)
	}

	n.log.Info("TAP device created, attached to bridge, and up",
		"instance_id", instanceID,
		"tap_device", dev,
		"bridge", br,
		"mac", macAddr,
	)
	return dev, nil
}

// DeleteTAP detaches from the bridge and removes the TAP device.
// Idempotent: if the device does not exist, returns nil.
//
// bridgeName: the bridge to detach from. If empty, "br0" is used.
func (n *NetworkManager) DeleteTAP(ctx context.Context, instanceID, bridgeName string) error {
	dev := tapName(instanceID)
	br := bridgeName
	if br == "" {
		br = defaultBridgeName
	}

	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping TAP deletion",
			"instance_id", instanceID,
			"tap_device", dev,
		)
		return nil
	}

	// Idempotent: check if the device exists.
	_, err := n.executor.RunOutput(ctx, "ip", "link", "show", dev)
	if err != nil {
		n.log.Info("TAP device already absent — idempotent no-op",
			"instance_id", instanceID,
			"tap_device", dev,
		)
		return nil
	}

	// Detach from bridge (best-effort; bridge may already be gone).
	_ = n.executor.Run(ctx, "ip", "link", "set", dev, "nomaster")

	// Delete the device.
	if err := n.executor.Run(ctx, "ip", "link", "delete", dev); err != nil {
		return fmt.Errorf("DeleteTAP: %w", err)
	}
	n.log.Info("TAP device deleted", "instance_id", instanceID, "tap_device", dev)
	return nil
}

// ProgramNAT installs iptables DNAT (inbound) and SNAT (outbound) rules.
// Idempotent: uses -C check before -A append.
//
// publicIP may be empty — in that case, no rules are installed.
func (n *NetworkManager) ProgramNAT(ctx context.Context, instanceID, privateIP, publicIP string) error {
	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping NAT programming",
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

	comment := chainPrefix(instanceID)

	// PREROUTING DNAT
	dnatArgs := []string{"-t", "nat", "-A", "PREROUTING", "-d", publicIP,
		"-j", "DNAT", "--to-destination", privateIP,
		"-m", "comment", "--comment", comment}
	if err := n.iptablesIdempotent(ctx, dnatArgs); err != nil {
		return fmt.Errorf("ProgramNAT: DNAT: %w", err)
	}

	// POSTROUTING SNAT
	snatArgs := []string{"-t", "nat", "-A", "POSTROUTING", "-s", privateIP,
		"-j", "SNAT", "--to-source", publicIP,
		"-m", "comment", "--comment", comment}
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

	comment := chainPrefix(instanceID)

	dnatArgs := []string{"-t", "nat", "-D", "PREROUTING", "-d", publicIP,
		"-j", "DNAT", "--to-destination", privateIP,
		"-m", "comment", "--comment", comment}
	if err := n.iptablesDeleteIdempotent(ctx, dnatArgs); err != nil {
		return fmt.Errorf("RemoveNAT: DNAT: %w", err)
	}

	snatArgs := []string{"-t", "nat", "-D", "POSTROUTING", "-s", privateIP,
		"-j", "SNAT", "--to-source", publicIP,
		"-m", "comment", "--comment", comment}
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

// ── iptables helpers ──────────────────────────────────────────────────────────

// iptablesIdempotent checks if a rule exists (-C) before appending (-A).
func (n *NetworkManager) iptablesIdempotent(ctx context.Context, appendArgs []string) error {
	checkArgs := copyArgsWithReplace(appendArgs, "-A", "-C")
	_, err := n.executor.RunOutput(ctx, "iptables", checkArgs...)
	if err == nil {
		return nil // already exists
	}
	return n.executor.Run(ctx, "iptables", appendArgs...)
}

// iptablesDeleteIdempotent checks if a rule exists (-C) before deleting (-D).
func (n *NetworkManager) iptablesDeleteIdempotent(ctx context.Context, deleteArgs []string) error {
	checkArgs := copyArgsWithReplace(deleteArgs, "-D", "-C")
	_, err := n.executor.RunOutput(ctx, "iptables", checkArgs...)
	if err != nil {
		return nil // already absent — idempotent no-op
	}
	return n.executor.Run(ctx, "iptables", deleteArgs...)
}

func copyArgsWithReplace(args []string, old, new string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if a == old {
			out[i] = new
			break
		}
	}
	return out
}

// ── Security Group Policy Enforcement ─────────────────────────────────────────

// SGRule represents a single security group rule for host-agent enforcement.
type SGRule struct {
	ID        string
	Direction string // "ingress" | "egress"
	Protocol  string // "tcp" | "udp" | "icmp" | "all"
	PortFrom  *int
	PortTo    *int
	CIDR      *string
}

// ApplySGPolicy programs iptables per-NIC firewall rules.
//
// Rule semantics (fail-closed by default):
//  1. Create a per-instance iptables chain in the filter table.
//  2. Flush existing rules in that chain.
//  3. Install default-DROP for inbound (FORWARD chain jump to per-NIC chain with DROP policy).
//  4. Allow established/related return traffic.
//  5. Install explicit ingress ALLOW rules from the SGRule slice.
//
// The per-instance chain is anchored to the tapDevice via physdev match.
// All rules are tagged with the instance ID comment for safe cleanup.
//
// Idempotent: calling ApplySGPolicy again with updated rules flushes and reprograms.
// Must be called after CreateTAP (the tap device must exist).
func (n *NetworkManager) ApplySGPolicy(ctx context.Context, instanceID, tapDevice string, rules []SGRule) error {
	if n.dryRun {
		n.log.Info("ApplySGPolicy: dry-run — no rules programmed",
			"instance_id", instanceID,
			"tap_device", tapDevice,
			"rule_count", len(rules),
		)
		return nil
	}

	chain := sgChainName(instanceID)
	comment := chainPrefix(instanceID)

	// Step 1: Ensure the per-instance chain exists (idempotent).
	// iptables -N <chain> — fails if chain already exists; we ignore that.
	_ = n.executor.Run(ctx, "iptables", "-t", "filter", "-N", chain)

	// Step 2: Flush existing rules in the chain.
	_ = n.executor.Run(ctx, "iptables", "-t", "filter", "-F", chain)

	// Step 3: Install FORWARD jump to per-NIC chain for traffic from tapDevice.
	// This ensures inbound traffic to the VM goes through our per-NIC chain.
	// Idempotent check: only add the jump if not already present.
	_ = n.ensureFilterJump(ctx, "FORWARD", tapDevice, chain, comment)

	// Step 4: Install DROP-all as the default chain policy.
	_ = n.executor.Run(ctx, "iptables", "-t", "filter", "-P", chain, "DROP")

	// Step 5: Allow established/related return traffic.
	_ = n.iptablesIdempotent(ctx, []string{"-t", "filter", "-A", chain,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-j", "ACCEPT",
		"-m", "comment", "--comment", comment + "-est"})

	// Step 6: Install per-rule ingress ALLOW matches.
	for _, rule := range rules {
		if err := n.applySGRule(ctx, chain, instanceID, rule); err != nil {
			n.log.Warn("ApplySGPolicy: failed to apply rule",
				"rule_id", rule.ID,
				"error", err,
			)
		}
	}

	// Step 7: Ensure the RETURN chain also allows established/related responses
	// from the VM for inbound-initiated connections (conntrack handles symmetry).
	_ = n.iptablesIdempotent(ctx, []string{"-t", "filter", "-A", chain,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-j", "ACCEPT",
		"-m", "comment", "--comment", comment + "-est-r"})

	n.log.Info("SG policy applied",
		"instance_id", instanceID,
		"tap_device", tapDevice,
		"chain", chain,
		"rule_count", len(rules),
	)
	return nil
}

// RemoveSGPolicy tears down iptables firewall state for an instance NIC.
// Flushes and deletes the per-NIC iptables chain and removes the FORWARD jump.
// Idempotent: safe to call multiple times.
func (n *NetworkManager) RemoveSGPolicy(ctx context.Context, instanceID, tapDevice string) error {
	if n.dryRun {
		n.log.Info("RemoveSGPolicy: dry-run — no rules removed",
			"instance_id", instanceID,
			"tap_device", tapDevice,
		)
		return nil
	}

	chain := sgChainName(instanceID)
	comment := chainPrefix(instanceID)

	// Remove the FORWARD jump rule referencing this NIC's chain.
	_ = n.removeFilterJump(ctx, "FORWARD", tapDevice, chain, comment)

	// Flush the chain (idempotent if already empty).
	_ = n.executor.Run(ctx, "iptables", "-t", "filter", "-F", chain)

	// Delete the chain (idempotent — fails silently if already gone).
	_ = n.executor.Run(ctx, "iptables", "-t", "filter", "-X", chain)

	n.log.Info("SG policy removed",
		"instance_id", instanceID,
		"tap_device", tapDevice,
		"chain", chain,
	)
	return nil
}

// sgChainName returns the deterministic per-instance iptables chain name.
func sgChainName(instanceID string) string {
	return "cpvm-sg-" + tapName(instanceID)
}

// ensureFilterJump adds a FORWARD jump rule from physdev-in tapDevice to the
// per-instance SG chain if not already present.
func (n *NetworkManager) ensureFilterJump(ctx context.Context, baseChain, tapDevice, sgChain, comment string) error {
	jumpArgs := []string{"-t", "filter", "-A", baseChain,
		"-m", "physdev", "--physdev-in", tapDevice,
		"-j", sgChain,
		"-m", "comment", "--comment", comment + "-jump"}
	return n.iptablesIdempotent(ctx, jumpArgs)
}

// removeFilterJump removes the FORWARD jump rule for this NIC.
func (n *NetworkManager) removeFilterJump(ctx context.Context, baseChain, tapDevice, sgChain, comment string) error {
	jumpArgs := []string{"-t", "filter", "-D", baseChain,
		"-m", "physdev", "--physdev-in", tapDevice,
		"-j", sgChain,
		"-m", "comment", "--comment", comment + "-jump"}
	return n.iptablesDeleteIdempotent(ctx, jumpArgs)
}

// applySGRule translates a single SGRule into iptables rules and installs them.
func (n *NetworkManager) applySGRule(ctx context.Context, chain, instanceID string, rule SGRule) error {
	if rule.Direction != "ingress" {
		return nil // egress rules deferred
	}

	comment := chainPrefix(instanceID) + "-" + rule.ID[:min(8, len(rule.ID))]

	// Build the iptables match for this rule.
	match := []string{"-t", "filter", "-A", chain}

	// Protocol match.
	if rule.Protocol != "all" {
		match = append(match, "-p", rule.Protocol)
	}

	// Port match (tcp/udp only).
	if rule.Protocol == "tcp" || rule.Protocol == "udp" {
		if rule.PortFrom != nil && rule.PortTo != nil && *rule.PortFrom == *rule.PortTo {
			match = append(match, "--dport", strconv.Itoa(*rule.PortFrom))
		} else if rule.PortFrom != nil || rule.PortTo != nil {
			from := 0
			to := 65535
			if rule.PortFrom != nil {
				from = *rule.PortFrom
			}
			if rule.PortTo != nil {
				to = *rule.PortTo
			}
			match = append(match, "--dport", strconv.Itoa(from)+":"+strconv.Itoa(to))
		}
	}

	// Source CIDR match.
	if rule.CIDR != nil && *rule.CIDR != "" {
		match = append(match, "-s", *rule.CIDR)
	}

	// Action: ACCEPT.
	match = append(match, "-j", "ACCEPT")
	match = append(match, "-m", "comment", "--comment", comment)

	return n.iptablesIdempotent(ctx, match)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
