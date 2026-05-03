package runtime

// network.go — TAP device creation/deletion, bridge attachment, and iptables NAT
// programming via an injectable Executor interface.
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
// Security group enforcement is in security_group.go.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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

// keep network.go TAP and NAT only — SG enforcement moved to security_group.go
