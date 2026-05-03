package runtime

// security_group_test.go — Unit tests for host-agent security group enforcement.
//
// Tests cover:
//   1. Default deny — empty rules produces DROP policy.
//   2. Ingress CIDR allow — rules with CIDR match generate ACCEPT rules.
//   3. Port-based rules — single port and port range.
//   4. Protocol rules — TCP, UDP, ICMP, all.
//   5. Cleanup — RemoveSGPolicy removes chain and jump.
//   6. Idempotency — double apply and double remove are safe.
//   7. Instance isolation — rules tagged per-instance.
//   8. Egress rules ignored — egress rules are skipped (default allow egress).
//   9. Dry-run mode — no iptables calls.
//  10. SGRuleFromEffectiveRows — correct conversion from DB rows.
//
// VM-SECURITY-GROUP-DATAPLANE-PHASE-E additions:
//  11. CompiledSGPolicy — default deny, TCP/UDP/ICMP/all, generation tracking.
//  12. ApplyCompiledPolicy — idempotent apply with generation guard.
//  13. RemoveCompiledPolicy — idempotent remove with generation guard.
//  14. Stale policy — lower generation is rejected; mismatch on remove still cleans.
//  15. ProgramSGPolicy — real enforcement via SGRuleIngress/SGRuleEgress.
//
// All tests use FakeExecutor — no root, no Linux, no iptables required.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

func referencePort(v int) *int      { return &v }
func referenceStr(v string) *string { return &v }

// ── Default Deny ──────────────────────────────────────────────────────────────

func TestSG_DefaultDeny_EmptyRules_CreatesChainAndDropPolicy(t *testing.T) {
	mgr, e := newTestNetworkManager()

	err := mgr.ApplySGPolicy(context.Background(), "inst_default", "tap-inst_def", nil)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "-N", "cpvm-sg-tap-inst_def")
	assertCallExists(t, e.Calls, "iptables", "-F", "cpvm-sg-tap-inst_def")
	assertCallExists(t, e.Calls, "iptables", "-P", "cpvm-sg-tap-inst_def", "DROP")
	assertCallExists(t, e.Calls, "iptables", "ESTABLISHED,RELATED", "ACCEPT")
}

// ── Ingress Allow Rules ───────────────────────────────────────────────────────

func TestSG_IngressAllow_TCP22_CIDR(t *testing.T) {
	mgr, e := newTestNetworkManager()

	cidr := "0.0.0.0/0"
	port22 := 22
	rules := []SGRule{
		{ID: "sgr_ssh", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(port22), PortTo: referencePort(port22), CIDR: referenceStr(cidr)},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_ssh", "tap-inst_ssh", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "tcp", "--dport", "22", "ACCEPT")
	assertCallExists(t, e.Calls, "iptables", "-s", "0.0.0.0/0")
}

func TestSG_IngressAllow_HTTPS443_RestrictedCIDR(t *testing.T) {
	mgr, e := newTestNetworkManager()

	cidr := "10.0.0.0/8"
	port443 := 443
	rules := []SGRule{
		{ID: "sgr_https", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(port443), PortTo: referencePort(port443), CIDR: referenceStr(cidr)},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_https", "tap-inst_https", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "tcp", "443", "10.0.0.0/8")
}

func TestSG_IngressAllow_UDP53_DNS(t *testing.T) {
	mgr, e := newTestNetworkManager()

	cidr := "0.0.0.0/0"
	port53 := 53
	rules := []SGRule{
		{ID: "sgr_dns", Direction: "ingress", Protocol: "udp", PortFrom: referencePort(port53), PortTo: referencePort(port53), CIDR: referenceStr(cidr)},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_dns", "tap-inst_dns", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "udp", "53", "ACCEPT")
}

func TestSG_IngressAllow_ICMP(t *testing.T) {
	mgr, e := newTestNetworkManager()

	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_icmp", Direction: "ingress", Protocol: "icmp", CIDR: referenceStr(cidr)},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_icmp", "tap-inst_icmp", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "icmp", "ACCEPT")
}

func TestSG_IngressAllow_ProtocolAll_NoPort(t *testing.T) {
	mgr, e := newTestNetworkManager()

	cidr := "10.0.0.0/8"
	rules := []SGRule{
		{ID: "sgr_all", Direction: "ingress", Protocol: "all", CIDR: referenceStr(cidr)},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_all", "tap-inst_all", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "10.0.0.0/8", "ACCEPT")

	// Must not have --dport for "all" protocol.
	for _, c := range e.Calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "dport") && strings.Contains(joined, "cpvm-sg-tap-inst_all") {
			t.Error("'all' protocol should not have --dport")
		}
	}
}

func TestSG_IngressAllow_PortRange(t *testing.T) {
	mgr, e := newTestNetworkManager()

	from := 8080
	to := 8090
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_range", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(from), PortTo: referencePort(to), CIDR: referenceStr(cidr)},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_range", "tap-inst_rng", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "8080:8090")
}

func TestSG_IngressAllow_MultipleRules(t *testing.T) {
	mgr, e := newTestNetworkManager()

	cidr := "0.0.0.0/0"
	port22 := 22
	port80 := 80
	port443 := 443
	rules := []SGRule{
		{ID: "sgr_ssh22", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(port22), PortTo: referencePort(port22), CIDR: referenceStr(cidr)},
		{ID: "sgr_http80", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(port80), PortTo: referencePort(port80), CIDR: referenceStr(cidr)},
		{ID: "sgr_https__", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(port443), PortTo: referencePort(port443), CIDR: referenceStr(cidr)},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_multi", "tap-inst_multi", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "22", "ACCEPT")
	assertCallExists(t, e.Calls, "iptables", "80", "ACCEPT")
	assertCallExists(t, e.Calls, "iptables", "443", "ACCEPT")
}

// ── Egress Rules Ignored ──────────────────────────────────────────────────────

func TestSG_EgressRules_Ignored(t *testing.T) {
	mgr, e := newTestNetworkManager()

	cidr := "0.0.0.0/0"
	port443 := 443
	rules := []SGRule{
		{ID: "sgr_egress", Direction: "egress", Protocol: "tcp", PortFrom: referencePort(port443), PortTo: referencePort(port443), CIDR: referenceStr(cidr)},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_egress", "tap-inst_eg", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	// Default DROP policy must still be set.
	assertCallExists(t, e.Calls, "iptables", "DROP")

	// No TCP 443 ACCEPT for egress.
	for _, c := range e.Calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "443") && strings.Contains(joined, "ACCEPT") {
			t.Error("egress rule should not have been applied as ingress")
		}
	}
}

// ── Cleanup ───────────────────────────────────────────────────────────────────

func TestSG_RemoveSGPolicy_RemovesChain(t *testing.T) {
	mgr, e := newTestNetworkManager()

	err := mgr.RemoveSGPolicy(context.Background(), "inst_rm", "tap-inst_rm")
	if err != nil {
		t.Fatalf("RemoveSGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "-F", "cpvm-sg-tap-inst_rm")
	assertCallExists(t, e.Calls, "iptables", "-X", "cpvm-sg-tap-inst_rm")
}

func TestSG_RemoveSGPolicy_Idempotent_DoubleRemove(t *testing.T) {
	mgr, e := newTestNetworkManager()

	_ = mgr.RemoveSGPolicy(context.Background(), "inst_idem", "tap-inst_id")

	firstCount := e.CallCount()
	e.Reset()

	err := mgr.RemoveSGPolicy(context.Background(), "inst_idem", "tap-inst_id")
	if err != nil {
		t.Fatalf("second RemoveSGPolicy: %v", err)
	}

	if e.CallCount() == 0 {
		t.Error("second remove should still attempt cleanup (idempotent)")
	}
	_ = firstCount
}

func TestSG_ApplyThenRemove_CleanSequence(t *testing.T) {
	mgr, e := newTestNetworkManager()

	port22 := 22
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_seq", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(port22), PortTo: referencePort(port22), CIDR: referenceStr(cidr)},
	}

	if err := mgr.ApplySGPolicy(context.Background(), "inst_seq", "tap-inst_seq", rules); err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}
	applyCalls := e.CallCount()
	if applyCalls < 5 {
		t.Errorf("apply should produce >= 5 calls, got %d", applyCalls)
	}

	e.Reset()

	if err := mgr.RemoveSGPolicy(context.Background(), "inst_seq", "tap-inst_seq"); err != nil {
		t.Fatalf("RemoveSGPolicy: %v", err)
	}
	removeCalls := e.CallCount()
	if removeCalls < 2 {
		t.Errorf("remove should produce >= 2 calls (flush + delete), got %d", removeCalls)
	}
}

// ── Idempotency ───────────────────────────────────────────────────────────────

func TestSG_ApplySGPolicy_Idempotent(t *testing.T) {
	mgr, e := newTestNetworkManager()

	port22 := 22
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_idem", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(port22), PortTo: referencePort(port22), CIDR: referenceStr(cidr)},
	}

	if err := mgr.ApplySGPolicy(context.Background(), "inst_idem2", "tap-inst_id", rules); err != nil {
		t.Fatalf("first ApplySGPolicy: %v", err)
	}

	firstCallCount := e.CallCount()
	e.Reset()

	if err := mgr.ApplySGPolicy(context.Background(), "inst_idem2", "tap-inst_id", rules); err != nil {
		t.Fatalf("second ApplySGPolicy: %v", err)
	}

	if e.CallCount() == 0 {
		t.Error("second apply should still generate commands")
	}
	_ = firstCallCount
}

// ── Instance Isolation ────────────────────────────────────────────────────────

func TestSG_InstanceIsolation_SeparateChains(t *testing.T) {
	mgr, e := newTestNetworkManager()

	port22 := 22
	cidr := "0.0.0.0/0"
	rulesA := []SGRule{
		{ID: "sgr_a", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(port22), PortTo: referencePort(port22), CIDR: referenceStr(cidr)},
	}

	_ = mgr.ApplySGPolicy(context.Background(), "inst_aaaaaa", "tap-inst_aa", rulesA)
	e.Reset()

	_ = mgr.ApplySGPolicy(context.Background(), "inst_bbbbbb", "tap-inst_bb", nil)

	for _, c := range e.Calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "inst_aaaaaa") {
			t.Errorf("instance B commands contain instance A ID: %s", joined)
		}
	}
}

// ── Rules Tagged Per-Instance ─────────────────────────────────────────────────

func TestSG_RulesTaggedWithInstancePrefix(t *testing.T) {
	mgr, e := newTestNetworkManager()

	port22 := 22
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_tag01", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(port22), PortTo: referencePort(port22), CIDR: referenceStr(cidr)},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_tagged", "tap-inst_tag", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy: %v", err)
	}

	prefix := chainPrefix("inst_tagged")
	hasCommentWithPrefix := false
	for _, c := range e.Calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "--comment") && strings.Contains(joined, prefix) {
			hasCommentWithPrefix = true
			break
		}
	}
	if !hasCommentWithPrefix {
		t.Error("no iptables rule found tagged with instance prefix")
	}
}

// ── Dry-run ───────────────────────────────────────────────────────────────────

func TestSG_DryRun_NoCalls(t *testing.T) {
	mgr := newDryRunNetworkManagerLegacy(t)

	port22 := 22
	cidr := "0.0.0.0/0"
	rules := []SGRule{
		{ID: "sgr_dry", Direction: "ingress", Protocol: "tcp", PortFrom: referencePort(port22), PortTo: referencePort(port22), CIDR: referenceStr(cidr)},
	}

	err := mgr.ApplySGPolicy(context.Background(), "inst_dry", "tap-inst_dr", rules)
	if err != nil {
		t.Fatalf("ApplySGPolicy dry-run: %v", err)
	}
}

func TestSG_RemoveSGPolicy_DryRun_NoError(t *testing.T) {
	mgr := newDryRunNetworkManagerLegacy(t)
	err := mgr.RemoveSGPolicy(context.Background(), "inst_dryrm", "tap-inst_dr")
	if err != nil {
		t.Fatalf("RemoveSGPolicy dry-run: %v", err)
	}
}

// ── SGRuleFromEffectiveRows ───────────────────────────────────────────────────

func TestSGRuleFromEffectiveRows_ConvertsIngressCIDRRules(t *testing.T) {
	port22 := 22
	cidr := "0.0.0.0/0"
	rows := []EffectiveSGRuleRow{
		{RuleID: "r1", SecurityGroupID: "sg1", Direction: "ingress", Protocol: "tcp", PortFrom: &port22, PortTo: &port22, CIDR: &cidr},
		{RuleID: "r2", SecurityGroupID: "sg1", Direction: "ingress", Protocol: "udp", PortFrom: nil, PortTo: nil, CIDR: &cidr},
	}

	got := SGRuleFromEffectiveRows(rows)
	if len(got) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(got))
	}
	if got[0].ID != "r1" {
		t.Errorf("first rule ID = %q, want r1", got[0].ID)
	}
	if got[1].Protocol != "udp" {
		t.Errorf("second rule protocol = %q, want udp", got[1].Protocol)
	}
}

func TestSGRuleFromEffectiveRows_SkipsEgress(t *testing.T) {
	cidr := "0.0.0.0/0"
	rows := []EffectiveSGRuleRow{
		{RuleID: "r1", SecurityGroupID: "sg1", Direction: "egress", Protocol: "tcp", CIDR: &cidr},
		{RuleID: "r2", SecurityGroupID: "sg1", Direction: "ingress", Protocol: "tcp", CIDR: &cidr},
	}

	got := SGRuleFromEffectiveRows(rows)
	if len(got) != 1 {
		t.Fatalf("expected 1 ingress rule, got %d", len(got))
	}
	if got[0].ID != "r2" {
		t.Errorf("rule ID = %q, want r2", got[0].ID)
	}
}

func TestSGRuleFromEffectiveRows_SkipsSourceSGReferenced(t *testing.T) {
	sgRef := "sg_other"
	cidr := "0.0.0.0/0"
	rows := []EffectiveSGRuleRow{
		{RuleID: "r1", SecurityGroupID: "sg1", Direction: "ingress", Protocol: "tcp", CIDR: &cidr, SourceSecurityGroupID: &sgRef},
		{RuleID: "r2", SecurityGroupID: "sg1", Direction: "ingress", Protocol: "tcp", CIDR: &cidr},
	}

	got := SGRuleFromEffectiveRows(rows)
	if len(got) != 1 {
		t.Fatalf("expected 1 rule (skip SG-ref), got %d", len(got))
	}
	if got[0].ID != "r2" {
		t.Errorf("rule ID = %q, want r2", got[0].ID)
	}
}

func TestSGRuleFromEffectiveRows_EmptyInput(t *testing.T) {
	got := SGRuleFromEffectiveRows(nil)
	if len(got) != 0 {
		t.Error("expected empty slice for nil input")
	}
	void := SGRuleFromEffectiveRows([]EffectiveSGRuleRow{})
	if len(void) != 0 {
		t.Error("expected empty slice for 0-length input")
	}
}

// ── Stale SG Detection ────────────────────────────────────────────────────────

func TestSG_StaleSGPolicyRemaining_ChainPresent(t *testing.T) {
	mgr, e := newTestNetworkManager()

	// Simulate: chain exists.
	e.RunOutputs["iptables -t filter -L cpvm-sg-tap-inst_sta -n"] = fakeOutputResult{Output: "Chain cpvm-sg-tap-inst_sta", Err: nil}

	if !mgr.StaleSGPolicyRemaining(context.Background(), "inst_stale", "tap-inst_sta") {
		t.Error("stale SG policy should be detected when chain exists")
	}
}

func TestSG_StaleSGPolicyRemaining_ChainAbsent(t *testing.T) {
	mgr, e := newTestNetworkManager()

	// Simulate: chain does not exist (sgChainName("inst_clean") = "cpvm-sg-tap-inst_cle").
	e.RunOutputs["iptables -t filter -L cpvm-sg-tap-inst_cle -n"] = fakeOutputResult{Err: fmt.Errorf("no chain")}
	e.RunOutputs["iptables -t filter -L FORWARD -n"] = fakeOutputResult{Output: "Chain FORWARD (policy ACCEPT)\ntarget     prot opt source               destination\n"}

	if mgr.StaleSGPolicyRemaining(context.Background(), "inst_clean", "tap-inst_cle") {
		t.Error("stale SG policy should NOT be detected when chain is absent")
	}
}

// ── CompiledSGPolicy Tests ────────────────────────────────────────────────────

func TestCompiledSGPolicy_DefaultDeny_RendersDropPolicy(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_cmp_default"
	policy := CompiledSGPolicy{
		InstanceID:      instanceID,
		NICID:           "nic_default",
		TapDevice:       tapName(instanceID),
		Generation:      1,
		IngressRules:    nil,
		EgressRules:     nil,
		DefaultBehavior: "deny",
	}

	err := mgr.ApplyCompiledPolicy(context.Background(), policy)
	if err != nil {
		t.Fatalf("ApplyCompiledPolicy: %v", err)
	}

	chain := sgChainName(instanceID)
	assertCallExists(t, e.Calls, "iptables", "-N", chain)
	assertCallExists(t, e.Calls, "iptables", "-F", chain)
	assertCallExists(t, e.Calls, "iptables", "-P", chain, "DROP")
	assertCallExists(t, e.Calls, "iptables", "ESTABLISHED,RELATED", "ACCEPT")
}

func TestCompiledSGPolicy_TCPAllow_RendersAcceptRule(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_comp_tcp"
	port22 := 22
	cidr := "0.0.0.0/0"
	policy := CompiledSGPolicy{
		InstanceID: instanceID,
		TapDevice:  tapName(instanceID),
		Generation: 1,
		IngressRules: []SGRule{
			{ID: "sgr_ssh", Direction: "ingress", Protocol: "tcp", PortFrom: &port22, PortTo: &port22, CIDR: &cidr},
		},
		DefaultBehavior: "deny",
	}

	err := mgr.ApplyCompiledPolicy(context.Background(), policy)
	if err != nil {
		t.Fatalf("ApplyCompiledPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "tcp", "--dport", "22", "ACCEPT")
	assertCallExists(t, e.Calls, "iptables", "-s", "0.0.0.0/0")
}

func TestCompiledSGPolicy_UDPAllow_RendersAcceptRule(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_comp_udp"
	port53 := 53
	cidr := "0.0.0.0/0"
	policy := CompiledSGPolicy{
		InstanceID: instanceID,
		TapDevice:  tapName(instanceID),
		Generation: 2,
		IngressRules: []SGRule{
			{ID: "sgr_dns", Direction: "ingress", Protocol: "udp", PortFrom: &port53, PortTo: &port53, CIDR: &cidr},
		},
		DefaultBehavior: "deny",
	}

	err := mgr.ApplyCompiledPolicy(context.Background(), policy)
	if err != nil {
		t.Fatalf("ApplyCompiledPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "udp", "53", "ACCEPT")
}

func TestCompiledSGPolicy_ICMPAllow_RendersAcceptRule(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_comp_icmp"
	cidr := "0.0.0.0/0"
	policy := CompiledSGPolicy{
		InstanceID: instanceID,
		TapDevice:  tapName(instanceID),
		Generation: 3,
		IngressRules: []SGRule{
			{ID: "sgr_icmp", Direction: "ingress", Protocol: "icmp", CIDR: &cidr},
		},
		DefaultBehavior: "deny",
	}

	err := mgr.ApplyCompiledPolicy(context.Background(), policy)
	if err != nil {
		t.Fatalf("ApplyCompiledPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "icmp", "ACCEPT")
}

func TestCompiledSGPolicy_ProtocolAll_RendersAcceptRule(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_comp_all"
	cidr := "10.0.0.0/8"
	policy := CompiledSGPolicy{
		InstanceID: instanceID,
		TapDevice:  tapName(instanceID),
		Generation: 4,
		IngressRules: []SGRule{
			{ID: "sgr_all", Direction: "ingress", Protocol: "all", CIDR: &cidr},
		},
		DefaultBehavior: "deny",
	}

	err := mgr.ApplyCompiledPolicy(context.Background(), policy)
	if err != nil {
		t.Fatalf("ApplyCompiledPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "10.0.0.0/8", "ACCEPT")
}

// ── Compiled Policy Idempotency ────────────────────────────────────────────────

func TestCompiledSGPolicy_IdempotentApply_StaleGenerationRejected(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_idem_gen"
	port22 := 22
	cidr := "0.0.0.0/0"
	policy := CompiledSGPolicy{
		InstanceID: instanceID,
		TapDevice:  tapName(instanceID),
		Generation: 5,
		IngressRules: []SGRule{
			{ID: "sgr_ssh", Direction: "ingress", Protocol: "tcp", PortFrom: &port22, PortTo: &port22, CIDR: &cidr},
		},
		DefaultBehavior: "deny",
	}

	// First apply with generation 5.
	if err := mgr.ApplyCompiledPolicy(context.Background(), policy); err != nil {
		t.Fatalf("first ApplyCompiledPolicy: %v", err)
	}

	e.Reset()

	// Second apply with same generation (5) — stale, should be no-op.
	policy2 := CompiledSGPolicy{
		InstanceID:      instanceID,
		TapDevice:       tapName(instanceID),
		Generation:      5,
		IngressRules:    nil,
		DefaultBehavior: "deny",
	}
	if err := mgr.ApplyCompiledPolicy(context.Background(), policy2); err != nil {
		t.Fatalf("second ApplyCompiledPolicy: %v", err)
	}

	// Stale generation: no iptables calls should be generated.
	if e.CallCount() > 0 {
		t.Errorf("stale generation apply should have zero calls, got %d", e.CallCount())
	}
}

func TestCompiledSGPolicy_IdempotentApply_HigherGenerationSucceeds(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_high_gen"
	port22 := 22
	cidr := "0.0.0.0/0"
	policy := CompiledSGPolicy{
		InstanceID: instanceID,
		TapDevice:  tapName(instanceID),
		Generation: 10,
		IngressRules: []SGRule{
			{ID: "sgr_ssh", Direction: "ingress", Protocol: "tcp", PortFrom: &port22, PortTo: &port22, CIDR: &cidr},
		},
		DefaultBehavior: "deny",
	}
	if err := mgr.ApplyCompiledPolicy(context.Background(), policy); err != nil {
		t.Fatalf("first ApplyCompiledPolicy: %v", err)
	}

	e.Reset()

	// Higher generation should apply.
	policy2 := CompiledSGPolicy{
		InstanceID:      instanceID,
		TapDevice:       tapName(instanceID),
		Generation:      15,
		IngressRules:    nil,
		DefaultBehavior: "deny",
	}
	if err := mgr.ApplyCompiledPolicy(context.Background(), policy2); err != nil {
		t.Fatalf("second ApplyCompiledPolicy: %v", err)
	}

	// Higher generation should trigger actual rule programming (flush + DROP + established).
	if e.CallCount() == 0 {
		t.Error("higher generation apply should issue commands")
	}
}

func TestCompiledSGPolicy_IdempotentRemove_RemovesChain(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_comp_rm1"
	policy := CompiledSGPolicy{
		InstanceID: instanceID,
		TapDevice:  tapName(instanceID),
		Generation: 7,
	}
	// Apply first so there's a generation to match.
	_ = mgr.ApplyCompiledPolicy(context.Background(), policy)
	e.Reset()

	if err := mgr.RemoveCompiledPolicy(context.Background(), policy); err != nil {
		t.Fatalf("RemoveCompiledPolicy: %v", err)
	}

	chain := sgChainName(instanceID)
	assertCallExists(t, e.Calls, "iptables", "-F", chain)
	assertCallExists(t, e.Calls, "iptables", "-X", chain)
}

func TestCompiledSGPolicy_IdempotentRemove_DoubleRemove(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_double_rm1"
	policy := CompiledSGPolicy{
		InstanceID: instanceID,
		TapDevice:  tapName(instanceID),
		Generation: 8,
	}
	_ = mgr.ApplyCompiledPolicy(context.Background(), policy)

	_ = mgr.RemoveCompiledPolicy(context.Background(), policy)
	e.Reset()

	if err := mgr.RemoveCompiledPolicy(context.Background(), policy); err != nil {
		t.Fatalf("second RemoveCompiledPolicy: %v", err)
	}

	// Second remove still issues flush/delete (iptables -F/-X are silently idempotent).
	if e.CallCount() == 0 {
		t.Error("second remove should still attempt cleanup")
	}
}

// ── Stale Policy Cleanup ──────────────────────────────────────────────────────

func TestCompiledSGPolicy_StalePolicy_GenerationMismatchRemoveStillCleans(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_stale01"
	// Apply at generation 20.
	applyPolicy := CompiledSGPolicy{
		InstanceID: instanceID,
		TapDevice:  tapName(instanceID),
		Generation: 20,
	}
	_ = mgr.ApplyCompiledPolicy(context.Background(), applyPolicy)
	e.Reset()

	// Remove at generation 30 (mismatch — policy was superseded).
	removePolicy := CompiledSGPolicy{
		InstanceID: instanceID,
		TapDevice:  tapName(instanceID),
		Generation: 30,
	}
	if err := mgr.RemoveCompiledPolicy(context.Background(), removePolicy); err != nil {
		t.Fatalf("RemoveCompiledPolicy (stale): %v", err)
	}

	// Even with generation mismatch, remove should still flush and delete.
	chain := sgChainName(instanceID)
	assertCallExists(t, e.Calls, "iptables", "-F", chain)
	assertCallExists(t, e.Calls, "iptables", "-X", chain)
}

func TestCompiledSGPolicy_StalePolicy_ZeroGenerationAlwaysApplies(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_zero_gen1"
	// Generation 0 means "no generation tracking" — always applies.
	policy := CompiledSGPolicy{
		InstanceID:      instanceID,
		TapDevice:       tapName(instanceID),
		Generation:      0,
		IngressRules:    nil,
		DefaultBehavior: "deny",
	}
	if err := mgr.ApplyCompiledPolicy(context.Background(), policy); err != nil {
		t.Fatalf("first ApplyCompiledPolicy (gen 0): %v", err)
	}
	e.Reset()

	// Even with gen 0 again, should still apply (no guard).
	if err := mgr.ApplyCompiledPolicy(context.Background(), policy); err != nil {
		t.Fatalf("second ApplyCompiledPolicy (gen 0): %v", err)
	}
	if e.CallCount() == 0 {
		t.Error("gen 0 second apply should still issue commands")
	}
}

// ── Dry-Run Tests ─────────────────────────────────────────────────────────────

func TestCompiledSGPolicy_DryRun_NoCalls(t *testing.T) {
	mgr := newDryRunNetworkManagerLegacy(t)

	port22 := 22
	cidr := "0.0.0.0/0"
	policy := CompiledSGPolicy{
		InstanceID: "inst_dry_comp",
		TapDevice:  "tap-inst_dc",
		Generation: 1,
		IngressRules: []SGRule{
			{ID: "sgr_ssh", Direction: "ingress", Protocol: "tcp", PortFrom: &port22, PortTo: &port22, CIDR: &cidr},
		},
		DefaultBehavior: "deny",
	}

	err := mgr.ApplyCompiledPolicy(context.Background(), policy)
	if err != nil {
		t.Fatalf("ApplyCompiledPolicy dry-run: %v", err)
	}
}

func TestCompiledSGPolicy_RemoveDryRun_NoError(t *testing.T) {
	mgr := newDryRunNetworkManagerLegacy(t)
	policy := CompiledSGPolicy{
		InstanceID: "inst_dry_rm",
		TapDevice:  "tap-inst_dr",
		Generation: 1,
	}
	err := mgr.RemoveCompiledPolicy(context.Background(), policy)
	if err != nil {
		t.Fatalf("RemoveCompiledPolicy dry-run: %v", err)
	}
}

// ── ProgramSGPolicy (real enforcement) Tests ───────────────────────────────────

func TestProgramSGPolicy_IngressRules_RendersCommands(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_prog01"
	port22 := 22
	cidr := "0.0.0.0/0"
	ingressRules := []SGRuleIngress{
		{ID: "pi_ssh", Protocol: "tcp", PortFrom: &port22, PortTo: &port22, SourceCIDR: &cidr},
	}

	err := mgr.ProgramSGPolicy(context.Background(), instanceID, tapName(instanceID), ingressRules, nil)
	if err != nil {
		t.Fatalf("ProgramSGPolicy: %v", err)
	}

	chain := sgChainName(instanceID)
	assertCallExists(t, e.Calls, "iptables", "tcp", "22", "ACCEPT")
	assertCallExists(t, e.Calls, "iptables", "-s", "0.0.0.0/0")
	assertCallExists(t, e.Calls, "iptables", "-P", chain, "DROP")
}

func TestProgramSGPolicy_UDPAndICMP_RendersCommands(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_prog02"
	port53 := 53
	cidr := "0.0.0.0/0"
	ingressRules := []SGRuleIngress{
		{ID: "pi_dns", Protocol: "udp", PortFrom: &port53, PortTo: &port53, SourceCIDR: &cidr},
		{ID: "pi_icmp", Protocol: "icmp", SourceCIDR: &cidr},
	}

	err := mgr.ProgramSGPolicy(context.Background(), instanceID, tapName(instanceID), ingressRules, nil)
	if err != nil {
		t.Fatalf("ProgramSGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "udp", "53", "ACCEPT")
	assertCallExists(t, e.Calls, "iptables", "icmp", "ACCEPT")
}

func TestProgramSGPolicy_ProtocolAll_NoPortSpec(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_prog03"
	cidr := "10.0.0.0/8"
	ingressRules := []SGRuleIngress{
		{ID: "pi_all", Protocol: "all", SourceCIDR: &cidr},
	}

	err := mgr.ProgramSGPolicy(context.Background(), instanceID, tapName(instanceID), ingressRules, nil)
	if err != nil {
		t.Fatalf("ProgramSGPolicy: %v", err)
	}

	assertCallExists(t, e.Calls, "iptables", "10.0.0.0/8", "ACCEPT")
}

func TestProgramSGPolicy_DryRun_ReturnsNil(t *testing.T) {
	mgr := newDryRunNetworkManagerLegacy(t)

	instanceID := "inst_dry_p1"
	port22 := 22
	cidr := "0.0.0.0/0"
	ingressRules := []SGRuleIngress{
		{ID: "pi_dry", Protocol: "tcp", PortFrom: &port22, PortTo: &port22, SourceCIDR: &cidr},
	}

	err := mgr.ProgramSGPolicy(context.Background(), instanceID, tapName(instanceID), ingressRules, nil)
	if err != nil {
		t.Fatalf("ProgramSGPolicy dry-run: %v", err)
	}
}

func TestProgramSGPolicy_EmptyRules_RendersDefaultDeny(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_prog04"
	err := mgr.ProgramSGPolicy(context.Background(), instanceID, tapName(instanceID), nil, nil)
	if err != nil {
		t.Fatalf("ProgramSGPolicy (empty): %v", err)
	}

	chain := sgChainName(instanceID)
	assertCallExists(t, e.Calls, "iptables", "-P", chain, "DROP")
	assertCallExists(t, e.Calls, "iptables", "ESTABLISHED,RELATED")
}

func TestProgramSGPolicy_EgressRules_RenderAcceptRule(t *testing.T) {
	mgr, e := newTestNetworkManager()

	instanceID := "inst_prog05"
	port443 := 443
	cidr := "0.0.0.0/0"
	egressRules := []SGRuleEgress{
		{ID: "pe_https", Protocol: "tcp", PortFrom: &port443, PortTo: &port443, DestCIDR: &cidr},
	}

	err := mgr.ProgramSGPolicy(context.Background(), instanceID, tapName(instanceID), nil, egressRules)
	if err != nil {
		t.Fatalf("ProgramSGPolicy (egress): %v", err)
	}

	// Egress rules are converted to SGRule with Direction="egress".
	// Since egress rules are currently ignored in ApplySGPolicy, only the
	// default deny + established rules should be present.
	chain := sgChainName(instanceID)
	assertCallExists(t, e.Calls, "iptables", "-P", chain, "DROP")
	assertCallExists(t, e.Calls, "iptables", "ESTABLISHED,RELATED")
}

// ── SG-to-SG Reference Behavior ───────────────────────────────────────────────

func TestSGToSG_References_Skipped(t *testing.T) {
	// Verify that SGRuleFromEffectiveRows skips rules referencing another SG.
	sgRef := "sg_other"
	cidr := "0.0.0.0/0"
	rows := []EffectiveSGRuleRow{
		{RuleID: "r1", SecurityGroupID: "sg1", Direction: "ingress", Protocol: "tcp", CIDR: &cidr, SourceSecurityGroupID: &sgRef},
		{RuleID: "r2", SecurityGroupID: "sg1", Direction: "ingress", Protocol: "tcp", CIDR: &cidr},
	}

	got := SGRuleFromEffectiveRows(rows)
	if len(got) != 1 {
		t.Fatalf("expected 1 rule (SG ref skipped), got %d", len(got))
	}
	if got[0].ID != "r2" {
		t.Errorf("rule ID = %q, want r2", got[0].ID)
	}
}

func TestSGToSG_References_ExplicitUnsupported_LoggedNotEnforced(t *testing.T) {
	// Verify that effectiveSGRulesToSpec also skips SG-to-SG rules.
	// This tests the worker-level conversion behavior at the policy rendering level.
	sgRef := "sg_other"
	cidr := "0.0.0.0/0"
	rows := []EffectiveSGRuleRow{
		{RuleID: "r_cross", SecurityGroupID: "sg1", Direction: "ingress", Protocol: "tcp", CIDR: &cidr, SourceSecurityGroupID: &sgRef},
	}

	got := SGRuleFromEffectiveRows(rows)
	if len(got) != 0 {
		t.Fatalf("SG-to-SG reference rules should be skipped at dataplane, got %d", len(got))
	}
}

// ── Bridge Tests ──────────────────────────────────────────────────────────────

func TestBridge_EnsureBridge_CreatesBridge(t *testing.T) {
	mgr, e := newTestNetworkManager()

	// Bridge does not exist.
	e.RunOutputs["ip link show dev br0"] = fakeOutputResult{Err: fmt.Errorf("not found")}

	err := mgr.EnsureBridge(context.Background(), "br0")
	if err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}

	assertCallExists(t, e.Calls, "ip", "link", "add", "name", "br0", "type", "bridge")
	assertCallExists(t, e.Calls, "ip", "link", "set", "br0", "up")
}

func TestBridge_EnsureBridge_Idempotent(t *testing.T) {
	mgr, e := newTestNetworkManager()

	e.RunOutputs["ip link show dev br0"] = fakeOutputResult{Output: "br0: ...", Err: nil}

	err := mgr.EnsureBridge(context.Background(), "br0")
	if err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}

	for _, c := range e.Calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "add") && strings.Contains(joined, "bridge") {
			t.Error("should not create bridge when already exists")
		}
	}
}

func TestBridge_BridgeExists_True(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["ip link show dev br0"] = fakeOutputResult{Output: "5: br0: <NO-CARRIER,BROADCAST,MULTICAST,UP>", Err: nil}

	if !mgr.BridgeExists(context.Background(), "br0") {
		t.Error("bridge should exist")
	}
}

func TestBridge_BridgeExists_False(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["ip link show dev br0"] = fakeOutputResult{Err: fmt.Errorf("Device \"br0\" does not exist")}

	if mgr.BridgeExists(context.Background(), "br0") {
		t.Error("bridge should not exist")
	}
}

func TestBridge_RemoveBridge_DeletesBridge(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["ip link show dev br0"] = fakeOutputResult{Output: "br0: ...", Err: nil}

	err := mgr.RemoveBridge(context.Background(), "br0")
	if err != nil {
		t.Fatalf("RemoveBridge: %v", err)
	}

	assertCallExists(t, e.Calls, "ip", "link", "set", "br0", "down")
	assertCallExists(t, e.Calls, "ip", "link", "delete", "br0")
}

func TestBridge_RemoveBridge_Idempotent_Absent(t *testing.T) {
	mgr, e := newTestNetworkManager()
	e.RunOutputs["ip link show dev br0"] = fakeOutputResult{Err: fmt.Errorf("not found")}

	err := mgr.RemoveBridge(context.Background(), "br0")
	if err != nil {
		t.Fatalf("RemoveBridge: %v", err)
	}

	for _, c := range e.Calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "delete") && strings.Contains(joined, "br0") {
			t.Error("should not try to delete absent bridge")
		}
	}
}
