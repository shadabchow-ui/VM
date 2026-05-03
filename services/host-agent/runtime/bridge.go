package runtime

// bridge.go — Host Linux bridge lifecycle for VM networking dataplane.
//
// Source: IP_ALLOCATION_CONTRACT_V1, RUNTIMESERVICE_GRPC_V1 §7.
//
// Phase 1 single-host networking uses a single Linux bridge (br0 by default).
// The bridge is the L2 interconnect between host-side TAP devices and the host
// network namespace's default route. All guest instances share this bridge.
//
// Operations:
//   EnsureBridge(ctx, bridgeName)     → creates bridge if absent, brings it up
//   BridgeExists(ctx, bridgeName)     → returns true if bridge device exists
//   RemoveBridge(ctx, bridgeName)     → tears down bridge (for tests only)

import (
	"context"
	"fmt"
	"strings"
)

// EnsureBridge creates the named Linux bridge if it does not already exist
// and brings it up. Idempotent: safe to call multiple times.
//
// Phase 1 default: bridgeName = "br0".
func (n *NetworkManager) EnsureBridge(ctx context.Context, bridgeName string) error {
	if bridgeName == "" {
		bridgeName = defaultBridgeName
	}

	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping bridge ensure", "bridge", bridgeName)
		return nil
	}

	if n.BridgeExists(ctx, bridgeName) {
		n.log.Info("bridge already exists", "bridge", bridgeName)
		return nil
	}

	// Create bridge.
	if err := n.executor.Run(ctx, "ip", "link", "add", "name", bridgeName, "type", "bridge"); err != nil {
		return fmt.Errorf("EnsureBridge: create bridge %s: %w", bridgeName, err)
	}

	// Bring bridge up.
	if err := n.executor.Run(ctx, "ip", "link", "set", bridgeName, "up"); err != nil {
		_ = n.executor.Run(ctx, "ip", "link", "delete", bridgeName)
		return fmt.Errorf("EnsureBridge: set bridge %s up: %w", bridgeName, err)
	}

	n.log.Info("bridge created and up", "bridge", bridgeName)
	return nil
}

// BridgeExists returns true if the named bridge device exists in the kernel.
func (n *NetworkManager) BridgeExists(ctx context.Context, bridgeName string) bool {
	out, err := n.executor.RunOutput(ctx, "ip", "link", "show", "dev", bridgeName)
	if err != nil {
		return false
	}
	return strings.Contains(out, bridgeName+":")
}

// RemoveBridge deletes the named bridge device. Used only by tests and safe
// cleanup helpers — never in production instance lifecycle paths.
func (n *NetworkManager) RemoveBridge(ctx context.Context, bridgeName string) error {
	if bridgeName == "" {
		bridgeName = defaultBridgeName
	}

	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping bridge removal", "bridge", bridgeName)
		return nil
	}

	if !n.BridgeExists(ctx, bridgeName) {
		return nil
	}

	// Bring bridge down first.
	_ = n.executor.Run(ctx, "ip", "link", "set", bridgeName, "down")

	if err := n.executor.Run(ctx, "ip", "link", "delete", bridgeName); err != nil {
		return fmt.Errorf("RemoveBridge: %w", err)
	}

	n.log.Info("bridge removed", "bridge", bridgeName)
	return nil
}
