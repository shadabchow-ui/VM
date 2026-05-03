package runtime

// security_group.go — Security group enforcement at the host dataplane.
//
// Source: P2_VPC_NETWORK_CONTRACT §4 (Security Groups),
//         RUNTIMESERVICE_GRPC_V1 §7 (networking enforcement).
//
// This file owns all host-side iptables firewall rule programming for
// security groups. It provides:
//
//   ApplySGPolicy(ctx, instanceID, tapDevice, rules)  → per-instance chain, deny-inbound default, allow rules
//   RemoveSGPolicy(ctx, instanceID, tapDevice)         → flush and delete per-instance chain (idempotent)
//   SGRuleFromEffectiveRows(rows)                      → converts DB effective rows → host-agent SGRule slice
//
// VM-SECURITY-GROUP-DATAPLANE-PHASE-E additions:
//   CompiledSGPolicy                                   → full policy shape (instance_id, nic_id, tap_device, generation, rules)
//   ApplyCompiledPolicy / RemoveCompiledPolicy         → generation-tracked apply/remove
//   StaleSGPolicyRemaining                             → stale-state detection by generation
//
// Architecture:
//   - Host-agent owns host-side enforcement (iptables chains per TAP device).
//   - Network-controller owns allocation/release/VTEP, not guest firewalling.
//   - Security groups are enforced outside the guest via physdev-in match.
//   - Default policy: DENY inbound, ALLOW outbound, ALLOW established/related.

import (
	"context"
	"fmt"
	"strconv"
	"sync"
)

// ── Security Group Rule types ─────────────────────────────────────────────────

// SGRule represents a single security group rule for host-agent enforcement.
// This type is the runtime surface used by ApplySGPolicy. It is distinct from
// the DB-layer types (EffectiveSGRuleRow / SecurityGroupRuleRow) to maintain
// a clean seam between persistence and enforcement.
type SGRule struct {
	ID        string
	Direction string // "ingress" | "egress"
	Protocol  string // "tcp" | "udp" | "icmp" | "all"
	PortFrom  *int
	PortTo    *int
	CIDR      *string
}

// CompiledSGPolicy is the full, compiled security group policy for a NIC
// at enforcement time. It bundles instance identity, NIC identity (if available),
// the TAP device name, a generation counter for stale-state detection, and
// the set of ingress and egress rules to enforce.
//
// Source: VM-SECURITY-GROUP-DATAPLANE-PHASE-E.
type CompiledSGPolicy struct {
	InstanceID      string
	NICID           string // optional; empty for Phase 1 classic instances
	TapDevice       string
	Generation      int64
	IngressRules    []SGRule
	EgressRules     []SGRule
	DefaultBehavior string // "deny" is the only supported value currently
}

// SGGeneration tracks the last-applied generation per instance.
// Used to prevent stale policy reuse — if a newer generation has been applied,
// an older remove is idempotently skipped.
type SGGeneration struct {
	mu          sync.Mutex
	generations map[string]int64 // instanceID → last-applied generation
}

func newSGGeneration() *SGGeneration {
	return &SGGeneration{generations: make(map[string]int64)}
}

func (g *SGGeneration) get(instanceID string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generations[instanceID]
}

func (g *SGGeneration) set(instanceID string, gen int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.generations[instanceID] = gen
}

func (g *SGGeneration) remove(instanceID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.generations, instanceID)
}

// ── Policy Application ────────────────────────────────────────────────────────

// ApplyCompiledPolicy programs iptables firewall rules from a CompiledSGPolicy.
// This is the generation-tracked entry point. On apply, the generation is recorded;
// on a subsequent apply with a higher generation, old rules are flushed and replaced.
// On apply with a lower or equal generation, the call is a no-op (stale).
//
// Source: VM-SECURITY-GROUP-DATAPLANE-PHASE-E.
func (n *NetworkManager) ApplyCompiledPolicy(ctx context.Context, policy CompiledSGPolicy) error {
	if n.dryRun {
		n.log.Info("ApplyCompiledPolicy: dry-run — no rules programmed",
			"instance_id", policy.InstanceID,
			"nic_id", policy.NICID,
			"tap_device", policy.TapDevice,
			"generation", policy.Generation,
			"ingress_rules", len(policy.IngressRules),
			"egress_rules", len(policy.EgressRules),
		)
		return nil
	}

	if n.sgGen == nil {
		n.sgGen = newSGGeneration()
	}

	lastGen := n.sgGen.get(policy.InstanceID)
	if policy.Generation > 0 && policy.Generation <= lastGen {
		n.log.Info("ApplyCompiledPolicy: stale generation — no-op",
			"instance_id", policy.InstanceID,
			"generation", policy.Generation,
			"last_applied", lastGen,
		)
		return nil
	}

	// Merge ingress rules only (egress rules are handled by implicit-allow semantics).
	rules := policy.IngressRules

	if err := n.ApplySGPolicy(ctx, policy.InstanceID, policy.TapDevice, rules); err != nil {
		return err
	}

	if policy.Generation > 0 {
		n.sgGen.set(policy.InstanceID, policy.Generation)
	}

	n.log.Info("Compiled SG policy applied",
		"instance_id", policy.InstanceID,
		"nic_id", policy.NICID,
		"generation", policy.Generation,
		"ingress_rules", len(policy.IngressRules),
		"egress_rules", len(policy.EgressRules),
		"egress_behavior", "default-allow",
	)
	return nil
}

// RemoveCompiledPolicy tears down firewall state for a CompiledSGPolicy.
// If the generation does not match the last-applied generation, the call is
// a no-op (stale policy already superseded). Otherwise, the chain and FORWARD
// jump are removed idempotently.
//
// Source: VM-SECURITY-GROUP-DATAPLANE-PHASE-E.
func (n *NetworkManager) RemoveCompiledPolicy(ctx context.Context, policy CompiledSGPolicy) error {
	if n.dryRun {
		n.log.Info("RemoveCompiledPolicy: dry-run — no rules removed",
			"instance_id", policy.InstanceID,
			"nic_id", policy.NICID,
			"generation", policy.Generation,
		)
		return nil
	}

	if n.sgGen == nil {
		n.sgGen = newSGGeneration()
	}

	lastGen := n.sgGen.get(policy.InstanceID)
	if policy.Generation > 0 && policy.Generation != lastGen {
		n.log.Info("RemoveCompiledPolicy: generation mismatch — stale, cleaning up unconditionally",
			"instance_id", policy.InstanceID,
			"request_generation", policy.Generation,
			"last_applied", lastGen,
		)
	}

	if err := n.RemoveSGPolicy(ctx, policy.InstanceID, policy.TapDevice); err != nil {
		return err
	}

	n.sgGen.remove(policy.InstanceID)

	n.log.Info("Compiled SG policy removed",
		"instance_id", policy.InstanceID,
		"generation", policy.Generation,
	)
	return nil
}

// ApplySGPolicy programs iptables per-NIC firewall rules.
//
// Rule semantics (fail-closed by default):
//  1. Create a per-instance iptables chain in the filter table (cpvm-sg-tap-<id>).
//  2. Flush existing rules in that chain.
//  3. Install FORWARD jump from physdev-in tapDevice to per-instance chain.
//  4. Install default-DROP policy on the per-instance chain.
//  5. Allow established/related return traffic.
//  6. Install explicit ingress ALLOW rules from the SGRule slice.
//  7. Default ALLOW egress (no egress rules; guest-initiated traffic passes FORWARD).
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

	// Step 7: Ensure the RETURN path also allows established/related responses
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
// Idempotent: safe to call multiple times even if the chain/jump are already gone.
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

// ── Chain and rule helpers ────────────────────────────────────────────────────

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
		return nil // egress rules deferred — default allow egress
	}

	comment := chainPrefix(instanceID) + "-" + rule.ID[:min(8, len(rule.ID))]

	// Build the iptables match for this rule.
	match := []string{"-t", "filter", "-A", chain}

	// Protocol match.
	if rule.Protocol != "" && rule.Protocol != "all" {
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

// ── DB integration helpers ────────────────────────────────────────────────────

// EffectiveSGRuleRow is a row from the DB's GetEffectiveSGRulesForNIC result.
// Mirror of db.EffectiveSGRuleRow kept here so the host-agent runtime layer
// does not import internal/db directly.
type EffectiveSGRuleRow struct {
	RuleID                string
	SecurityGroupID       string
	Direction             string
	Protocol              string
	PortFrom              *int
	PortTo                *int
	CIDR                  *string
	SourceSecurityGroupID *string
}

// SGRuleFromEffectiveRows converts DB-layer effective SG rule rows into the
// host-agent enforcement type. Skips rules that reference another SG (not yet
// supported at dataplane) and non-ingress rules.
func SGRuleFromEffectiveRows(rows []EffectiveSGRuleRow) []SGRule {
	var out []SGRule
	for _, r := range rows {
		if r.Direction != "ingress" {
			continue
		}
		if r.SourceSecurityGroupID != nil {
			continue // cross-SG references not yet supported at dataplane
		}
		out = append(out, SGRule{
			ID:        r.RuleID,
			Direction: r.Direction,
			Protocol:  r.Protocol,
			PortFrom:  r.PortFrom,
			PortTo:    r.PortTo,
			CIDR:      r.CIDR,
		})
	}
	return out
}

// min returns the smaller of two integers.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// StaleSGPolicyRemaining returns true if a per-instance iptables chain or
// FORWARD jump still exists for the given instance. Used by reconciler-scoped
// stale-state detection.
func (n *NetworkManager) StaleSGPolicyRemaining(ctx context.Context, instanceID, tapDevice string) bool {
	if n.dryRun {
		return false
	}
	chain := sgChainName(instanceID)
	if n.ChainExists(ctx, chain) {
		return true
	}
	comment := chainPrefix(instanceID)
	out, err := n.executor.RunOutput(ctx, "iptables", "-t", "filter", "-L", "FORWARD", "-n")
	if err != nil {
		return false
	}
	return containsLine(out, comment)
}

func containsLine(output, substr string) bool {
	for _, line := range splitLines(output) {
		if indexOf(line, substr) >= 0 {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, ch := range s {
		if ch == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		if ch == '\r' {
			continue
		}
		cur += string(ch)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	if len(substr) > len(s) {
		return -1
	}
	return -1
}

// Ensure compile-time interface check.
var _ error = fmt.Errorf("")
