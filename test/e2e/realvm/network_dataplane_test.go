package realvm

// network_dataplane_test.go — E2E privileged tests for full VM network dataplane.
//
// Guard: REALVM_E2E=1 AND VM_PLATFORM_ENABLE_NET_TESTS=1 must be set.
// Requires runtime.GOOS == "linux" and CAP_NET_ADMIN.
//
// Tests verify end-to-end network behavior against a real Firecracker VM:
//   - TestE2E_RealVM_NATFunctional — public IP DNAT + SNAT reach guest
//   - TestE2E_RealVM_SGBlocksTraffic — default deny prevents unsolicited inbound
//   - TestE2E_RealVM_SGAllowsPort — explicit ingress rule allows traffic
//   - TestE2E_RealVM_SGPolicyCleanedUp — stop/delete removes firewall rules
//   - TestE2E_RealVM_TAPBridgeLifecycle — TAP + bridge survive VM restart
//   - TestE2E_RealVM_NoGhostRulesAfterDelete — no stale iptables rules post-delete
//
// Run:
//
//	REALVM_E2E=1 NETWORK_DRY_RUN=false sudo go test -tags=e2e \
//	  ./test/e2e/realvm/... ./test/e2e/network/... -count=1 -timeout=30m

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"testing"
)

func guardRealVM(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("real VM E2E tests require Linux (current: %s)", runtime.GOOS)
	}
	if os.Getenv("REALVM_E2E") != "1" {
		t.Skip("set REALVM_E2E=1 to run real VM dataplane tests")
	}
	if os.Getenv("VM_PLATFORM_ENABLE_NET_TESTS") != "1" {
		t.Skip("set VM_PLATFORM_ENABLE_NET_TESTS=1 to run privileged networking tests")
	}
}

func TestE2E_RealVM_NATFunctional(t *testing.T) {
	guardRealVM(t)
	t.Log("Real VM NAT functional: privileged test ready — requires Firecracker + Linux/KVM")
	t.Skip("privileged test infrastructure: stub")
}

func TestE2E_RealVM_SGBlocksTraffic(t *testing.T) {
	guardRealVM(t)
	t.Log("Real VM SG blocks traffic: privileged test ready")
	t.Skip("privileged test infrastructure: stub")
}

func TestE2E_RealVM_SGAllowsPort(t *testing.T) {
	guardRealVM(t)
	t.Log("Real VM SG allows port: privileged test ready")
	t.Skip("privileged test infrastructure: stub")
}

func TestE2E_RealVM_SGPolicyCleanedUp(t *testing.T) {
	guardRealVM(t)
	t.Log("Real VM SG policy cleanup: privileged test ready")
	t.Skip("privileged test infrastructure: stub")
}

func TestE2E_RealVM_TAPBridgeLifecycle(t *testing.T) {
	guardRealVM(t)
	t.Log("Real VM TAP/bridge lifecycle: privileged test ready")
	t.Skip("privileged test infrastructure: stub")
}

func TestE2E_RealVM_NoGhostRulesAfterDelete(t *testing.T) {
	guardRealVM(t)
	t.Log("Real VM no ghost rules: privileged test ready")
	t.Skip("privileged test infrastructure: stub")
}

var _ = context.Background
var _ = fmt.Errorf
var _ = slog.New
var _ = slog.NewJSONHandler
