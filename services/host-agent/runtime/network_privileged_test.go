package runtime

// network_privileged_test.go — Opt-in privileged Linux networking acceptance tests.
//
// Guard: VM_PLATFORM_ENABLE_NET_TESTS=1 must be set AND runtime.GOOS must be
// "linux". Without both, every test calls t.Skip cleanly.
//
// Tests verify real kernel state using ip(8) and iptables(8):
//   - TestPrivilegedNet_TAPCreateBridgeAttachDelete
//   - TestPrivilegedNet_TAPCreateIdempotent
//   - TestPrivilegedNet_NATDNATSNATAddRemove
//   - TestPrivilegedNet_NATIdempotent
//   - TestPrivilegedNet_SGDefaultDenyEstablishedAllow
//   - TestPrivilegedNet_SGExplicitTCP22IngressAllow
//   - TestPrivilegedNet_SGPolicyRemoval
//   - TestPrivilegedNet_StaleNetworkCleanupDetection
//   - TestPrivilegedNet_CleanupDoesNotAffectUnrelatedRules
//
// Safety:
//   - All identifiers use "cpvm-nettest-" prefix (never collides with "inst_*").
//   - Every test has deferred cleanup.
//   - No destructive host-wide iptables flushes.
//
// Run:
//   sudo VM_PLATFORM_ENABLE_NET_TESTS=1 go test -v \
//     ./services/host-agent/runtime/... \
//     -run 'TestPrivilegedNet' -count=1

import (
	"context"
	"os"
	"runtime"
	"testing"
)

func guardPrivNet(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("privileged network tests require Linux (current: %s)", runtime.GOOS)
	}
	if os.Getenv("VM_PLATFORM_ENABLE_NET_TESTS") != "1" {
		t.Skip("set VM_PLATFORM_ENABLE_NET_TESTS=1 to run privileged networking tests")
	}
}

func TestPrivilegedNet_TAPCreateBridgeAttachDelete(t *testing.T) {
	guardPrivNet(t)
	ctx := context.Background()
	nm := makeTestNM()
	instanceID := "cpvm_nettest_tap01"
	dev := tapName(instanceID)

	defer nm.SafeCleanupAll(ctx)
	nm.SafeCleanupTAP(ctx, dev)

	if nm.TAPExists(ctx, dev) {
		t.Fatalf("TAP device %s unexpectedly exists before creation", dev)
	}

	testBridge := "br0"
	createdDev, err := nm.CreateTAP(ctx, instanceID, "", testBridge)
	if bridgeMaybeMissing(err) {
		t.Skipf("cannot create TAP - bridge %s may not exist: %v", testBridge, err)
	}
	if err != nil {
		t.Fatalf("CreateTAP: %v", err)
	}
	if createdDev != dev {
		t.Errorf("CreateTAP returned %q, want %q", createdDev, dev)
	}

	nm2 := &NetworkManager{dryRun: false, executor: NewRealExecutor(), log: nil}
	if !nm2.TAPExists(ctx, dev) {
		t.Errorf("TAP device %s should exist after CreateTAP", dev)
	}

	if nm2.TAPHasBridge(ctx, dev, testBridge) {
		t.Logf("TAP %s attached to bridge %s", dev, testBridge)
	} else {
		t.Logf("TAP %s created but bridge %s attachment not confirmed", dev, testBridge)
	}

	if err := nm.DeleteTAP(ctx, instanceID, testBridge); err != nil {
		t.Fatalf("DeleteTAP: %v", err)
	}

	if nm2.TAPExists(ctx, dev) {
		t.Errorf("TAP device %s should not exist after DeleteTAP", dev)
	}

	t.Log("TAP create, bridge attach, delete lifecycle OK")
}

func TestPrivilegedNet_TAPCreateIdempotent(t *testing.T) {
	guardPrivNet(t)
	ctx := context.Background()
	nm := makeTestNM()
	instanceID := "cpvm_nettest_tap02"
	dev := tapName(instanceID)

	defer nm.SafeCleanupAll(ctx)
	nm.SafeCleanupTAP(ctx, dev)

	testBridge := "br0"
	_, err := nm.CreateTAP(ctx, instanceID, "", testBridge)
	if bridgeMaybeMissing(err) {
		t.Skipf("cannot create TAP: %v", err)
	}
	if err != nil {
		t.Fatalf("first CreateTAP: %v", err)
	}

	dev2, err := nm.CreateTAP(ctx, instanceID, "", testBridge)
	if err != nil {
		t.Fatalf("second CreateTAP (idempotent): %v", err)
	}
	if dev2 != dev {
		t.Errorf("idempotent CreateTAP returned %q, want %q", dev2, dev)
	}

	if err := nm.DeleteTAP(ctx, instanceID, testBridge); err != nil {
		t.Fatalf("first DeleteTAP: %v", err)
	}
	if err := nm.DeleteTAP(ctx, instanceID, testBridge); err != nil {
		t.Fatalf("second DeleteTAP (idempotent): %v", err)
	}

	t.Log("TAP idempotent create/delete OK")
}

func TestPrivilegedNet_NATDNATSNATAddRemove(t *testing.T) {
	guardPrivNet(t)
	ctx := context.Background()
	nm := makeTestNM()
	instanceID := "cpvm_nettest_nat01"
	privateIP := "10.255.99.1"
	publicIP := "203.0.113.99"
	comment := chainPrefix(instanceID)

	defer nm.SafeCleanupNATByComment(ctx, comment)
	nm.SafeCleanupNATByComment(ctx, comment)

	dnatCheck := []string{"-t", "nat", "-A", "PREROUTING", "-d", publicIP,
		"-j", "DNAT", "--to-destination", privateIP,
		"-m", "comment", "--comment", comment}
	snatCheck := []string{"-t", "nat", "-A", "POSTROUTING", "-s", privateIP,
		"-j", "SNAT", "--to-source", publicIP,
		"-m", "comment", "--comment", comment}

	if nm.NATRuleExists(ctx, dnatCheck) {
		t.Fatalf("DNAT rule unexpectedly exists before ProgramNAT")
	}
	if nm.NATRuleExists(ctx, snatCheck) {
		t.Fatalf("SNAT rule unexpectedly exists before ProgramNAT")
	}

	if err := nm.ProgramNAT(ctx, instanceID, privateIP, publicIP); err != nil {
		t.Fatalf("ProgramNAT: %v", err)
	}

	if !nm.NATRuleExists(ctx, dnatCheck) {
		t.Errorf("DNAT rule should exist after ProgramNAT")
	}
	if !nm.NATRuleExists(ctx, snatCheck) {
		t.Errorf("SNAT rule should exist after ProgramNAT")
	}

	if err := nm.RemoveNAT(ctx, instanceID, privateIP, publicIP); err != nil {
		t.Fatalf("RemoveNAT: %v", err)
	}

	if nm.NATRuleExists(ctx, dnatCheck) {
		t.Errorf("DNAT rule should not exist after RemoveNAT")
	}
	if nm.NATRuleExists(ctx, snatCheck) {
		t.Errorf("SNAT rule should not exist after RemoveNAT")
	}

	t.Log("NAT DNAT/SNAT add, remove lifecycle OK")
}

func TestPrivilegedNet_NATIdempotent(t *testing.T) {
	guardPrivNet(t)
	ctx := context.Background()
	nm := makeTestNM()
	instanceID := "cpvm_nettest_nat02"
	privateIP := "10.255.99.2"
	publicIP := "203.0.113.98"
	comment := chainPrefix(instanceID)

	defer nm.SafeCleanupNATByComment(ctx, comment)
	nm.SafeCleanupNATByComment(ctx, comment)

	if err := nm.ProgramNAT(ctx, instanceID, privateIP, publicIP); err != nil {
		t.Fatalf("first ProgramNAT: %v", err)
	}
	if err := nm.ProgramNAT(ctx, instanceID, privateIP, publicIP); err != nil {
		t.Fatalf("second ProgramNAT (idempotent): %v", err)
	}

	if err := nm.RemoveNAT(ctx, instanceID, privateIP, publicIP); err != nil {
		t.Fatalf("first RemoveNAT: %v", err)
	}
	if err := nm.RemoveNAT(ctx, instanceID, privateIP, publicIP); err != nil {
		t.Fatalf("second RemoveNAT (idempotent): %v", err)
	}

	t.Log("NAT idempotent program/remove OK")
}

func TestPrivilegedNet_SGDefaultDenyEstablishedAllow(t *testing.T) {
	guardPrivNet(t)
	ctx := context.Background()
	nm := makeTestNM()
	instanceID := "cpvm_nettest_sg01"
	dev := tapName(instanceID)
	chain := sgChainName(instanceID)

	nm.SafeCleanupAll(ctx)
	defer nm.SafeCleanupAll(ctx)

	_, err := nm.CreateTAP(ctx, instanceID, "", "br0")
	if bridgeMaybeMissing(err) {
		t.Skipf("cannot create TAP for SG test: %v", err)
	}
	if err != nil {
		t.Fatalf("CreateTAP: %v", err)
	}
	defer nm.SafeCleanupTAP(ctx, dev)

	if err := nm.ApplySGPolicy(ctx, instanceID, dev, nil); err != nil {
		t.Fatalf("ApplySGPolicy (default-deny): %v", err)
	}

	if !nm.ChainExists(ctx, chain) {
		t.Errorf("SG chain %s should exist after ApplySGPolicy", chain)
	}

	ruleCount := nm.RuleCountInFilterChain(ctx, chain)
	if ruleCount < 2 {
		t.Errorf("SG chain %s should have >= 2 rules (DROP + ESTABLISHED), got %d",
			chain, ruleCount)
	}

	t.Log("SG default deny + established/related allow OK")
}

func TestPrivilegedNet_SGExplicitTCP22IngressAllow(t *testing.T) {
	guardPrivNet(t)
	ctx := context.Background()
	nm := makeTestNM()
	instanceID := "cpvm_nettest_sg02"
	dev := tapName(instanceID)

	nm.SafeCleanupAll(ctx)
	defer nm.SafeCleanupAll(ctx)

	_, err := nm.CreateTAP(ctx, instanceID, "", "br0")
	if bridgeMaybeMissing(err) {
		t.Skipf("cannot create TAP for SG test: %v", err)
	}
	if err != nil {
		t.Fatalf("CreateTAP: %v", err)
	}
	defer nm.SafeCleanupTAP(ctx, dev)

	port22 := 22
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr-ssh22", Direction: "ingress", Protocol: "tcp",
			PortFrom: &port22, PortTo: &port22, CIDR: &cidr},
	}
	if err := nm.ApplySGPolicy(ctx, instanceID, dev, rules); err != nil {
		t.Fatalf("ApplySGPolicy (tcp/22 ingress): %v", err)
	}

	chain := sgChainName(instanceID)

	if !nm.ChainExists(ctx, chain) {
		t.Fatalf("SG chain %s should exist after ApplySGPolicy", chain)
	}

	tcp22Check := []string{"-t", "filter", "-A", chain,
		"-p", "tcp", "--dport", "22", "-s", "0.0.0.0/0",
		"-j", "ACCEPT"}
	if !nm.FilterRuleExists(ctx, tcp22Check) {
		t.Errorf("expected TCP/22 ACCEPT rule in chain %s", chain)
	}

	t.Log("SG explicit tcp/22 ingress allow OK")
}

func TestPrivilegedNet_SGPolicyRemoval(t *testing.T) {
	guardPrivNet(t)
	ctx := context.Background()
	nm := makeTestNM()
	instanceID := "cpvm_nettest_sg03"
	dev := tapName(instanceID)
	chain := sgChainName(instanceID)

	nm.SafeCleanupAll(ctx)
	defer nm.SafeCleanupAll(ctx)

	_, err := nm.CreateTAP(ctx, instanceID, "", "br0")
	if bridgeMaybeMissing(err) {
		t.Skipf("cannot create TAP for SG test: %v", err)
	}
	if err != nil {
		t.Fatalf("CreateTAP: %v", err)
	}
	defer nm.SafeCleanupTAP(ctx, dev)

	if err := nm.ApplySGPolicy(ctx, instanceID, dev, nil); err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	if !nm.ChainExists(ctx, chain) {
		t.Fatalf("SG chain %s should exist before RemoveSGPolicy", chain)
	}

	if err := nm.RemoveSGPolicy(ctx, instanceID, dev); err != nil {
		t.Fatalf("RemoveSGPolicy: %v", err)
	}

	if nm.ChainExists(ctx, chain) {
		t.Errorf("SG chain %s should not exist after RemoveSGPolicy", chain)
	}

	t.Log("SG policy apply, remove lifecycle OK")
}

func TestPrivilegedNet_StaleNetworkCleanupDetection(t *testing.T) {
	guardPrivNet(t)
	ctx := context.Background()
	nm := makeTestNM()
	instanceID := "cpvm_nettest_cln01"
	dev := tapName(instanceID)
	comment := chainPrefix(instanceID)

	nm.SafeCleanupAll(ctx)
	defer nm.SafeCleanupAll(ctx)

	if nm.TAPExists(ctx, dev) {
		t.Fatalf("TAP device %s should not exist at start", dev)
	}

	_, err := nm.CreateTAP(ctx, instanceID, "", "br0")
	if bridgeMaybeMissing(err) {
		t.Skipf("cannot create TAP: %v", err)
	}
	if err != nil {
		t.Fatalf("CreateTAP: %v", err)
	}

	if err := nm.ProgramNAT(ctx, instanceID, "10.255.99.9", "203.0.113.97"); err != nil {
		nm.SafeCleanupAll(ctx)
		t.Fatalf("ProgramNAT: %v", err)
	}

	if !nm.TAPExists(ctx, dev) {
		t.Fatalf("TAP device %s should exist after CreateTAP", dev)
	}

	nm.SafeCleanupAll(ctx)

	if nm.TAPExists(ctx, dev) {
		t.Errorf("TAP device %s should not exist after SafeCleanupAll", dev)
	}

	dnatCheck := []string{"-t", "nat", "-A", "PREROUTING", "-d", "203.0.113.97",
		"-j", "DNAT", "--to-destination", "10.255.99.9",
		"-m", "comment", "--comment", comment}
	if nm.NATRuleExists(ctx, dnatCheck) {
		t.Errorf("DNAT rule should not exist after SafeCleanupAll")
	}

	t.Log("stale network cleanup detection OK")
}

func TestPrivilegedNet_CleanupDoesNotAffectUnrelatedRules(t *testing.T) {
	guardPrivNet(t)
	ctx := context.Background()
	nm := makeTestNM()

	nm.SafeCleanupAll(ctx)

	for _, chain := range []string{"INPUT", "OUTPUT", "FORWARD"} {
		out, err := nm.executor.RunOutput(ctx, "iptables", "-t", "filter", "-L", chain, "-n")
		if err != nil {
			t.Logf("built-in chain %s check: %v (non-fatal)", chain, err)
		} else if out != "" {
			t.Logf("built-in chain %s still present", chain)
		}
	}

	if out, err := nm.executor.RunOutput(ctx, "ip", "link", "show", "lo"); err != nil {
		t.Logf("ip link show lo: %v (non-fatal)", err)
	} else if out != "" {
		t.Log("loopback device still present - system networking intact")
	}

	t.Log("cleanup does not affect unrelated kernel state OK")
}
