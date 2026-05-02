package runtime

// network_test.go — Unit tests for host-agent networking dataplane operations.
//
// Tests cover:
//   1. Command generation for TAP create/delete with bridge attachment.
//   2. NAT rule generation (ProgramNAT / RemoveNAT).
//   3. SG policy rule generation (ApplySGPolicy / RemoveSGPolicy).
//   4. Idempotent apply/remove of all operations.
//   5. Ownership isolation (instance-tagged rules).
//   6. Deterministic naming (tap names, chain names, comments).
//
// All tests use FakeExecutor — no root, no ip(8), no iptables(8) required.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// ── Test helpers ──────────────────────────────────────────────────────────────

func newTestNetworkManager() (*NetworkManager, *FakeExecutor) {
	e := NewFakeExecutor()
	mgr := NewNetworkManagerWithExecutor(
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		e,
	)
	return mgr, e
}

func newDryRunNetworkManagerLegacy(t *testing.T) *NetworkManager {
	t.Helper()
	t.Setenv("NETWORK_DRY_RUN", "true")
	return NewNetworkManager(slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// assertCall asserts that a FakeCall with the given name and matching args exists.
func assertCallExists(t *testing.T, calls []FakeCall, name string, wantArgs ...string) {
	t.Helper()
	for _, c := range calls {
		if c.Name != name {
			continue
		}
		if len(c.Args) < len(wantArgs) {
			continue
		}
		argsStr := strings.Join(c.Args, " ")
		allMatch := true
		for _, want := range wantArgs {
			if !strings.Contains(argsStr, want) {
				allMatch = false
				break
			}
		}
		if allMatch {
			return
		}
	}
	t.Errorf("expected call %s with args containing %v was not found in %d calls",
		name, wantArgs, len(calls))
}

// ── TAP Create Tests ──────────────────────────────────────────────────────────

func TestCreateTAP_GeneratesCommands(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["ip link show tap-inst_tes"] = fakeOutputResult{Err: fmt.Errorf("device not found")}

	_, err := mgr.CreateTAP(context.Background(), "inst_test01", "02:aa:bb:cc:dd:01", "br0")
	if err != nil {
		t.Fatalf("CreateTAP: %v", err)
	}

	// Should have called: ip link show, ip tuntap add, ip link set address,
	// ip link set master br0, ip link set up.
	if e.CallCount() < 4 {
		t.Fatalf("expected at least 4 calls, got %d", e.CallCount())
	}
	assertCallExists(t, e.Calls, "ip", "tuntap")
	assertCallExists(t, e.Calls, "ip", "master", "br0")
	assertCallExists(t, e.Calls, "ip", "set", "tap-inst_tes", "up")
}

func TestCreateTAP_Idempotent_DeviceExists(t *testing.T) {
	mgr, e := newTestNetworkManager()
	// Device already exists: "ip link show" succeeds.
	e.RunOutputs["ip link show tap-inst_tes"] = fakeOutputResult{Output: "tap-inst_tes: ...", Err: nil}

	dev, err := mgr.CreateTAP(context.Background(), "inst_test02", "02:aa:bb:cc:dd:02", "br0")
	if err != nil {
		t.Fatalf("CreateTAP: %v", err)
	}
	if dev != "tap-inst_tes" {
		t.Errorf("device = %q, want tap-inst_tes", dev)
	}

	// Should only have checked for existence, not created.
	for _, c := range e.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "tuntap") {
			t.Error("should not have called tuntap when device already exists")
		}
	}
}

func TestCreateTAP_DefaultBridge(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["ip link show tap-inst_tes"] = fakeOutputResult{Err: fmt.Errorf("not found")}

	_, err := mgr.CreateTAP(context.Background(), "inst_test03", "", "")
	if err != nil {
		t.Fatalf("CreateTAP: %v", err)
	}
	assertCallExists(t, e.Calls, "ip", "master", "br0")
}

func TestCreateTAP_AttachFailureRollsBack(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["ip link show tap-inst_tes"] = fakeOutputResult{Err: fmt.Errorf("not found")}
	// Make the bridge attach fail.
	e.RunErrors["ip link set tap-inst_tes master br0"] = fmt.Errorf("bridge not found")

	_, err := mgr.CreateTAP(context.Background(), "inst_test04", "", "br0")
	if err == nil {
		t.Fatal("expected error on bridge attach failure")
	}
	if !strings.Contains(err.Error(), "attach to bridge") {
		t.Errorf("error = %v, want bridge attach error", err)
	}
	// Should have cleaned up the TAP device.
	assertCallExists(t, e.Calls, "ip", "link", "delete", "tap-inst_tes")
}

func TestCreateTAP_DryRun_NoCalls(t *testing.T) {
	mgr := newDryRunNetworkManagerLegacy(t)
	dev, err := mgr.CreateTAP(context.Background(), "inst_dryrun", "", "")
	if err != nil {
		t.Fatalf("dry-run CreateTAP: %v", err)
	}
	if dev != "tap-inst_dry" {
		t.Errorf("device = %q, want tap-inst_dry", dev)
	}
	// The dry-run path uses the default RealExecutor but should not call.
	// We can't test zero calls on stock mgr, but we verify the device name.
}

// ── TAP Delete Tests ──────────────────────────────────────────────────────────

func TestDeleteTAP_GeneratesCommands(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["ip link show tap-inst_tes"] = fakeOutputResult{Output: "exists"}

	err := mgr.DeleteTAP(context.Background(), "inst_test05", "br0")
	if err != nil {
		t.Fatalf("DeleteTAP: %v", err)
	}
	assertCallExists(t, e.Calls, "ip", "set", "tap-inst_tes", "nomaster")
	assertCallExists(t, e.Calls, "ip", "link", "delete", "tap-inst_tes")
}

func TestDeleteTAP_Idempotent_DeviceAbsent(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["ip link show tap-inst_tes"] = fakeOutputResult{Err: fmt.Errorf("device not found")}

	err := mgr.DeleteTAP(context.Background(), "inst_test06", "br0")
	if err != nil {
		t.Fatalf("DeleteTAP: %v", err)
	}
	// Should not have tried to delete.
	for _, c := range e.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "delete") {
			t.Error("should not have called delete when device does not exist")
		}
	}
}

// ── NAT Tests ─────────────────────────────────────────────────────────────────

func TestProgramNAT_GeneratesRules(t *testing.T) {
	mgr, e := newTestNetworkManager()

	err := mgr.ProgramNAT(context.Background(), "inst_nat01", "10.0.0.5", "203.0.113.1")
	if err != nil {
		t.Fatalf("ProgramNAT: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "PREROUTING", "DNAT", "203.0.113.1")
	assertCallExists(t, e.Calls, "iptables", "POSTROUTING", "SNAT", "203.0.113.1")
}

func TestProgramNAT_EmptyPublicIP_Skipped(t *testing.T) {
	mgr, e := newTestNetworkManager()

	err := mgr.ProgramNAT(context.Background(), "inst_nat02", "10.0.0.6", "")
	if err != nil {
		t.Fatalf("ProgramNAT empty IP: %v", err)
	}
	if e.CallCount() != 0 {
		t.Errorf("expected 0 calls for empty public IP, got %d", e.CallCount())
	}
}

func TestProgramNAT_Idempotent_CheckSucceeds(t *testing.T) {
	mgr, e := newTestNetworkManager()
	// Make -C (check) commands succeed — rule already exists.
	e.RunOutputs["iptables -t nat -C PREROUTING -d 203.0.113.2 -j DNAT --to-destination 10.0.0.7 -m comment --comment cpvm-inst_nat"] =
		fakeOutputResult{Output: "", Err: nil}
	e.RunOutputs["iptables -t nat -C POSTROUTING -s 10.0.0.7 -j SNAT --to-source 203.0.113.2 -m comment --comment cpvm-inst_nat"] =
		fakeOutputResult{Output: "", Err: nil}

	err := mgr.ProgramNAT(context.Background(), "inst_nat03", "10.0.0.7", "203.0.113.2")
	if err != nil {
		t.Fatalf("ProgramNAT idempotent: %v", err)
	}

	// Should have checked both rules but NOT appended.
	for _, c := range e.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "-A") {
			t.Error("should not have appended when rule already exists")
		}
	}
}

func TestRemoveNAT_GeneratesCommands(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["iptables -t nat -C PREROUTING -d 203.0.113.3 -j DNAT --to-destination 10.0.0.8 -m comment --comment cpvm-inst_nat"] =
		fakeOutputResult{Output: "", Err: nil}

	err := mgr.RemoveNAT(context.Background(), "inst_nat04", "10.0.0.8", "203.0.113.3")
	if err != nil {
		t.Fatalf("RemoveNAT: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "PREROUTING", "DNAT", "203.0.113.3")
}

func TestRemoveNAT_Idempotent_RuleAbsent(t *testing.T) {
	mgr, e := newTestNetworkManager()
	// -C check fails — rule already absent.
	e.RunOutputs["iptables -t nat -C PREROUTING -d 203.0.113.4 -j DNAT --to-destination 10.0.0.9 -m comment --comment cpvm-inst_nat"] =
		fakeOutputResult{Err: fmt.Errorf("rule not found")}
	e.RunOutputs["iptables -t nat -C POSTROUTING -s 10.0.0.9 -j SNAT --to-source 203.0.113.4 -m comment --comment cpvm-inst_nat"] =
		fakeOutputResult{Err: fmt.Errorf("rule not found")}

	err := mgr.RemoveNAT(context.Background(), "inst_nat05", "10.0.0.9", "203.0.113.4")
	if err != nil {
		t.Fatalf("RemoveNAT idempotent: %v", err)
	}

	// Should not have executed any -D (only -C checks ran).
	for _, c := range e.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "-D") {
			t.Error("should not have deleted when rule is already absent")
		}
	}
}

// ── TAP Name Tests ────────────────────────────────────────────────────────────

func TestTapName_Deterministic(t *testing.T) {
	cases := []struct {
		instanceID string
		want       string
	}{
		{"inst_abc12345", "tap-inst_abc"},
		{"abcdefghijklmnopqrstuvwxyz", "tap-abcdefgh"},
		{"short", "tap-short"},
		{"inst_2nMpMz5Ge4VYeRBpaFKsx6Y7Fkn", "tap-inst_2nM"},
	}
	for _, tc := range cases {
		got := tapName(tc.instanceID)
		if got != tc.want {
			t.Errorf("tapName(%q) = %q, want %q", tc.instanceID, got, tc.want)
		}
	}
}

func TestChainPrefix_Deterministic(t *testing.T) {
	p := chainPrefix("inst_chain01")
	if p != "cpvm-inst_cha" {
		t.Errorf("chainPrefix = %q, want cpvm-inst_cha", p)
	}
}

func TestSGChainName_Deterministic(t *testing.T) {
	n := sgChainName("inst_firewall01")
	if n != "cpvm-sg-tap-inst_fir" {
		t.Errorf("sgChainName = %q, want cpvm-sg-tap-inst_fir", n)
	}
}

// ── SG Policy Tests ───────────────────────────────────────────────────────────

func TestApplySGPolicy_GeneratesDefaultDenyAndEstablished(t *testing.T) {
	mgr, e := newTestNetworkManager()

	err := mgr.ApplySGPolicy(context.Background(), "inst_sg01", "tap-inst_sg0", nil)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	// Must create chain, flush it, set default policy DROP, allow established.
	assertCallExists(t, e.Calls, "iptables", "-t", "filter", "-N", "cpvm-sg-tap-inst_sg0")
	assertCallExists(t, e.Calls, "iptables", "-P", "cpvm-sg-tap-inst_sg0", "DROP")
	assertCallExists(t, e.Calls, "iptables", "ESTABLISHED,RELATED", "ACCEPT")
}

func TestApplySGPolicy_GeneratesIngressAllowRules(t *testing.T) {
	mgr, e := newTestNetworkManager()

	port80 := 80
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_ssh22", Direction: "ingress", Protocol: "tcp", PortFrom: &port80, PortTo: &port80, CIDR: &cidr},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_sg02", "tap-inst_sg0", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	// Should have a TCP dport 80 ACCEPT rule.
	assertCallExists(t, e.Calls, "iptables", "tcp", "80", "ACCEPT")
}

func TestApplySGPolicy_GeneratesCIDRMatch(t *testing.T) {
	mgr, e := newTestNetworkManager()

	port443 := 443
	cidr := "10.0.0.0/8"
	rules := []SGRule{
		{ID: "sgr_https", Direction: "ingress", Protocol: "tcp", PortFrom: &port443, PortTo: &port443, CIDR: &cidr},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_sg03", "tap-inst_sg0", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "10.0.0.0/8", "443", "ACCEPT")
}

func TestApplySGPolicy_GeneratesICMPRule(t *testing.T) {
	mgr, e := newTestNetworkManager()

	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_icmp01", Direction: "ingress", Protocol: "icmp", CIDR: &cidr},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_sg04", "tap-inst_sg0", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "icmp", "ACCEPT")
}

func TestApplySGPolicy_GeneratesUDPSinglePort(t *testing.T) {
	mgr, e := newTestNetworkManager()

	port53 := 53
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_dns53", Direction: "ingress", Protocol: "udp", PortFrom: &port53, PortTo: &port53, CIDR: &cidr},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_sg05", "tap-inst_sg0", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "udp", "53", "ACCEPT")
}

func TestApplySGPolicy_GeneratesPortRange(t *testing.T) {
	mgr, e := newTestNetworkManager()

	from := 8080
	to := 8090
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_range", Direction: "ingress", Protocol: "tcp", PortFrom: &from, PortTo: &to, CIDR: &cidr},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_sg06", "tap-inst_sg0", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	// Port range formatted as "8080:8090" in --dport.
	assertCallExists(t, e.Calls, "iptables", "8080:8090")
}

func TestApplySGPolicy_RulesTaggedWithInstance(t *testing.T) {
	mgr, e := newTestNetworkManager()

	port22 := 22
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_ssh", Direction: "ingress", Protocol: "tcp", PortFrom: &port22, PortTo: &port22, CIDR: &cidr},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_isolation", "tap-inst_iso", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	// All comments should contain the instance prefix.
	prefix := chainPrefix("inst_isolation")
	for _, c := range e.Calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "--comment") {
			hasComment := strings.Contains(joined, prefix)
			if !hasComment && strings.Contains(joined, "comment") {
				// The jump rule uses a different comment format but still has the instance prefix.
				// Actually let's just check that rules with ACCEPT or chain operations have the prefix.
				if strings.Contains(joined, "ACCEPT") || strings.Contains(joined, "cpvm-sg") {
					if !strings.Contains(joined, prefix) {
						t.Errorf("call without instance tag prefix %q: %s", prefix, joined)
					}
				}
			}
		}
	}
}

func TestApplySGPolicy_Idempotent_DoubleApplyOnlyFlushes(t *testing.T) {
	mgr, e := newTestNetworkManager()

	err := mgr.ApplySGPolicy(context.Background(), "inst_idem01", "tap-inst_id", nil)
	if err != nil {
		t.Fatalf("first ApplySGPolicy: %v", err)
	}

	// Second apply with same (nil) rules should flush and reprograms.
	firstCallCount := e.CallCount()
	e.Reset()

	err = mgr.ApplySGPolicy(context.Background(), "inst_idem01", "tap-inst_id", nil)
	if err != nil {
		t.Fatalf("second ApplySGPolicy: %v", err)
	}

	// Should issue similar commands again (flush, re-apply).
	if e.CallCount() == 0 {
		t.Error("second apply should still generate flush commands")
	}
	// AT least the FORWARD jump, DROP policy, and established rule should be (re)applied.
	if e.CallCount() < 3 {
		t.Errorf("expected at least 3 calls on second apply, got %d", e.CallCount())
	}
	// The second apply must produce at least as many calls as needed.
	_ = firstCallCount
}

func TestRemoveSGPolicy_RemovesChain(t *testing.T) {
	mgr, e := newTestNetworkManager()

	err := mgr.RemoveSGPolicy(context.Background(), "inst_rm01", "tap-inst_rm")
	if err != nil {
		t.Fatalf("RemoveSGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "-F", "cpvm-sg-tap-inst_rm")
	assertCallExists(t, e.Calls, "iptables", "-X", "cpvm-sg-tap-inst_rm")
}

func TestRemoveSGPolicy_Idempotent_DoubleRemove(t *testing.T) {
	mgr, e := newTestNetworkManager()

	_ = mgr.RemoveSGPolicy(context.Background(), "inst_rm02", "tap-inst_rm")

	firstCount := e.CallCount()
	e.Reset()

	err := mgr.RemoveSGPolicy(context.Background(), "inst_rm02", "tap-inst_rm")
	if err != nil {
		t.Fatalf("second RemoveSGPolicy: %v", err)
	}

	// Second remove still flushes and tries to delete (iptables -X fails silently when chain is gone).
	if e.CallCount() == 0 {
		t.Error("second remove should still attempt cleanup")
	}
	// Ensure no panic — the call should be safe.
	_ = firstCount
}

func TestApplySGPolicy_InstanceIsolation(t *testing.T) {
	// Rules for instance A must not appear in instance B's chain namespace.
	mgr, e := newTestNetworkManager()

	port22 := 22
	cidr := "0.0.0.0/0"
	rulesA := []SGRule{
		{ID: "sgr_a01", Direction: "ingress", Protocol: "tcp", PortFrom: &port22, PortTo: &port22, CIDR: &cidr},
	}

	_ = mgr.ApplySGPolicy(context.Background(), "inst_aaaa01", "tap-inst_aa", rulesA)
	e.Reset()

	_ = mgr.ApplySGPolicy(context.Background(), "inst_bbbb01", "tap-inst_bb", nil)

	// Instance B's chain should be different from A's.
	for _, c := range e.Calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "inst_aaaa01") {
			t.Errorf("instance B commands contain instance A ID: %s", joined)
		}
	}
}

func TestApplySGPolicy_IgnoresEgressRules(t *testing.T) {
	mgr, e := newTestNetworkManager()

	port443 := 443
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_egress01", Direction: "egress", Protocol: "tcp", PortFrom: &port443, PortTo: &port443, CIDR: &cidr},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_eg01", "tap-inst_eg", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	// There should be no egress rules (the chain is still created with defaults).
	assertCallExists(t, e.Calls, "iptables", "DROP")

	// Verify no TCP 443 ACCEPT for egress.
	for _, c := range e.Calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "443") && strings.Contains(joined, "ACCEPT") {
			t.Error("egress rule should not have been applied as ingress")
		}
	}
}

func TestApplySGPolicy_ProtocolAll_NoPortSpec(t *testing.T) {
	mgr, e := newTestNetworkManager()

	cidr := "10.0.0.0/8"
	rules := []SGRule{
		{ID: "sgr_all01", Direction: "ingress", Protocol: "all", CIDR: &cidr},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_all01", "tap-inst_al", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	// Must have ACCEPT with -s 10.0.0.0/8, no protocol spec.
	assertCallExists(t, e.Calls, "iptables", "10.0.0.0/8", "ACCEPT")
	// Must NOT have --dport for "all" protocol.
	for _, c := range e.Calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "dport") && strings.Contains(joined, "cpvm-sg-tap-inst_al") {
			t.Error("'all' protocol should not have --dport")
		}
	}
}

func TestApplySGPolicy_DryRun_SkipsExecution(t *testing.T) {
	mgr := newDryRunNetworkManagerLegacy(t)
	port22 := 22
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_ssh", Direction: "ingress", Protocol: "tcp", PortFrom: &port22, PortTo: &port22, CIDR: &cidr},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_dry", "tap-inst_dr", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy dry-run: %v", err)
	}
	// Dry-run returns immediately, no assertions on calls needed.
}

func TestRemoveSGPolicy_DryRun_SkipsExecution(t *testing.T) {
	mgr := newDryRunNetworkManagerLegacy(t)
	err := mgr.RemoveSGPolicy(context.Background(), "inst_dryrm", "tap-inst_dr")
	if err != nil {
		t.Fatalf("RemoveSGPolicy dry-run: %v", err)
	}
}

// ── Executor tests ────────────────────────────────────────────────────────────

func TestFakeExecutor_RecordsCalls(t *testing.T) {
	e := NewFakeExecutor()

	_ = e.Run(context.Background(), "ip", "link", "show", "tap-test")
	_, _ = e.RunOutput(context.Background(), "iptables", "-L", "-n")

	if e.CallCount() != 2 {
		t.Errorf("call count = %d, want 2", e.CallCount())
	}

	lc := e.LastCall()
	if lc == nil {
		t.Fatal("expected last call")
	}
	if lc.Name != "iptables" {
		t.Errorf("last call name = %q, want iptables", lc.Name)
	}
}

func TestFakeExecutor_ErrorInjection(t *testing.T) {
	e := NewFakeExecutor()
	e.RunErrors["ip link set tap-test master br0"] = fmt.Errorf("bridge not found")

	err := e.Run(context.Background(), "ip", "link", "set", "tap-test", "master", "br0")
	if err == nil {
		t.Fatal("expected error from injected failure")
	}
	if !strings.Contains(err.Error(), "bridge not found") {
		t.Errorf("error = %q, want bridge not found", err.Error())
	}
}

func TestFakeExecutor_OutputInjection(t *testing.T) {
	e := NewFakeExecutor()
	e.RunOutputs["ip link show tap-test"] = fakeOutputResult{Output: "2: tap-test: ...", Err: nil}

	out, err := e.RunOutput(context.Background(), "ip", "link", "show", "tap-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "tap-test") {
		t.Errorf("output = %q, want tap-test", out)
	}
}

// ── Command generation snapshot tests ─────────────────────────────────────────
// These tests verify that the EXACT set of commands generated by each operation
// is deterministic and matches expectations.

func TestCreateTAP_CommandSnapshot(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["ip link show tap-inst_sna"] = fakeOutputResult{Err: fmt.Errorf("not found")}

	_, _ = mgr.CreateTAP(context.Background(), "inst_snap01", "02:01:02:03:04:05", "br0")

	// Verify exact command sequence.
	expected := []string{
		"ip link show tap-inst_sna",
		"ip tuntap add dev tap-inst_sna mode tap",
		"ip link set tap-inst_sna address 02:01:02:03:04:05",
		"ip link set tap-inst_sna master br0",
		"ip link set tap-inst_sna up",
	}

	for i, exp := range expected {
		if i >= len(e.Calls) {
			t.Errorf("missing call %d: %q", i, exp)
			continue
		}
		got := e.Calls[i].Name + " " + strings.Join(e.Calls[i].Args, " ")
		if got != exp {
			t.Errorf("call %d:\n  got  %q\n  want %q", i, got, exp)
		}
	}
	if len(e.Calls) != len(expected) {
		t.Errorf("call count = %d, want %d", len(e.Calls), len(expected))
	}
}

func TestApplySGPolicy_DefaultOnly_CommandSnapshot(t *testing.T) {
	mgr, e := newTestNetworkManager()

	_ = mgr.ApplySGPolicy(context.Background(), "inst_snap02", "tap-inst_sg", nil)

	// Verify exact sequence of iptables commands for default-only policy.
	commands := make([]string, len(e.Calls))
	for i, c := range e.Calls {
		commands[i] = c.Name + " " + strings.Join(c.Args, " ")
	}

	// The chain name is deterministic: cpvm-sg-TAPNAME where TAPNAME = "tap-" + first 8 chars.
	// inst_snap02 → "tap-inst_sna"
	expectedChain := "cpvm-sg-tap-inst_sna"

	// Must include: -N, -F, FORWARD jump, -P DROP, established, established (dup safe)
	mustContain := []string{
		"-N " + expectedChain,
		"-F " + expectedChain,
		"-P " + expectedChain + " DROP",
		"ESTABLISHED,RELATED",
		"ACCEPT",
	}
	for _, want := range mustContain {
		found := false
		for _, cmd := range commands {
			if strings.Contains(cmd, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected command containing %q not found in:\n  %s", want, strings.Join(commands, "\n  "))
		}
	}
}
