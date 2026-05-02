package runtime

// network_dryrun_test.go — Unit tests for NETWORK_DRY_RUN mode.
//
// Verifies that when NETWORK_DRY_RUN=true:
//   - CreateTAP returns a valid tap name without calling ip(8)
//   - DeleteTAP returns nil without calling ip(8)
//   - ProgramNAT returns nil without calling iptables(8)
//   - RemoveNAT returns nil without calling iptables(8)
//
// No ip(8), no iptables(8), no root, no Linux kernel required.
// Run: go test ./services/host-agent/runtime/... -run TestNetworkDryRun -v

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func newDryRunNetworkManager(t *testing.T) *NetworkManager {
	t.Helper()
	t.Setenv("NETWORK_DRY_RUN", "true")
	mgr := NewNetworkManager(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if !mgr.dryRun {
		t.Fatal("NETWORK_DRY_RUN=true but dryRun field is false")
	}
	return mgr
}

// TestNetworkDryRun_CreateTAP_ReturnsDeviceName verifies that CreateTAP in
// dry-run mode returns the deterministic tap name without invoking ip(8).
func TestNetworkDryRun_CreateTAP_ReturnsDeviceName(t *testing.T) {
	mgr := newDryRunNetworkManager(t)

	dev, err := mgr.CreateTAP(context.Background(), "inst_taptest01", "02:aa:bb:cc:dd:ee", "")
	if err != nil {
		t.Fatalf("CreateTAP dry-run: %v", err)
	}
	if dev == "" {
		t.Error("expected non-empty tap device name, got empty string")
	}
	// Tap name format: tap-<first 8 chars of instanceID>
	if !strings.HasPrefix(dev, "tap-") {
		t.Errorf("tap device name %q should start with tap-", dev)
	}
}

// TestNetworkDryRun_CreateTAP_Deterministic verifies the returned tap name
// matches tapName() — the same name Firecracker config will receive.
func TestNetworkDryRun_CreateTAP_Deterministic(t *testing.T) {
	mgr := newDryRunNetworkManager(t)
	instanceID := "inst_abc12345xyz"

	dev, err := mgr.CreateTAP(context.Background(), instanceID, "", "")
	if err != nil {
		t.Fatalf("CreateTAP: %v", err)
	}
	want := tapName(instanceID)
	if dev != want {
		t.Errorf("CreateTAP returned %q, want %q", dev, want)
	}
}

// TestNetworkDryRun_DeleteTAP_NoError verifies DeleteTAP is a no-op in dry-run mode.
func TestNetworkDryRun_DeleteTAP_NoError(t *testing.T) {
	mgr := newDryRunNetworkManager(t)

	if err := mgr.DeleteTAP(context.Background(), "inst_del01", ""); err != nil {
		t.Errorf("DeleteTAP dry-run: %v", err)
	}
}

// TestNetworkDryRun_ProgramNAT_NoError verifies ProgramNAT is a no-op.
func TestNetworkDryRun_ProgramNAT_NoError(t *testing.T) {
	mgr := newDryRunNetworkManager(t)

	if err := mgr.ProgramNAT(context.Background(), "inst_nat01", "10.0.0.5", "1.2.3.4"); err != nil {
		t.Errorf("ProgramNAT dry-run: %v", err)
	}
}

// TestNetworkDryRun_ProgramNAT_EmptyPublicIP_NoError verifies the existing
// empty-publicIP fast-path still works in dry-run mode.
func TestNetworkDryRun_ProgramNAT_EmptyPublicIP_NoError(t *testing.T) {
	mgr := newDryRunNetworkManager(t)

	if err := mgr.ProgramNAT(context.Background(), "inst_noip", "10.0.0.6", ""); err != nil {
		t.Errorf("ProgramNAT dry-run empty public IP: %v", err)
	}
}

// TestNetworkDryRun_RemoveNAT_NoError verifies RemoveNAT is a no-op.
func TestNetworkDryRun_RemoveNAT_NoError(t *testing.T) {
	mgr := newDryRunNetworkManager(t)

	if err := mgr.RemoveNAT(context.Background(), "inst_rmnat01", "10.0.0.7", "1.2.3.4"); err != nil {
		t.Errorf("RemoveNAT dry-run: %v", err)
	}
}

// TestTapName_Format verifies the tap name derivation logic used by both
// CreateTAP (production) and its dry-run path.
func TestTapName_Format(t *testing.T) {
	cases := []struct {
		instanceID string
		want       string
	}{
		{"inst_abc12345", "tap-inst_abc"}, // first 8 chars
		{"abcdefghijklmnopqrstuvwxyz", "tap-abcdefgh"},
		{"short", "tap-short"}, // shorter than 8 → use as-is
	}
	for _, c := range cases {
		got := tapName(c.instanceID)
		if got != c.want {
			t.Errorf("tapName(%q) = %q, want %q", c.instanceID, got, c.want)
		}
	}
}
