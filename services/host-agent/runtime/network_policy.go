package runtime

// network_policy.go — VM-P3A Job 2: Network policy maturity host-agent seam.
//
// This file extends the host-agent with:
//   1. A richer SGRule type that separates ingress and egress rule slices
//      for deterministic per-direction nftables chain naming.
//   2. ProgramSGPolicy — replaces ApplySGPolicy with per-direction chain semantics
//      and explicit deny-all baseline. The implementation is still a stub, but
//      the chain naming is deterministic and the data model is ready for
//      production nftables wiring.
//   3. GetSGChainNames — returns the canonical nftables chain names for a NIC
//      so callers can reference the chains in routing rules.
//   4. PublicIPState — describes the current public-IP / NAT state of an instance
//      so the worker can call ProgramNAT / RemoveNAT with a single, typed call.
//   5. ApplyPublicIPState — wraps ProgramNAT / RemoveNAT based on a diff between
//      the old and new PublicIPState, preventing double-programming.
//
// Ownership:
//   - resource-manager: SG CRUD, rule admission, NIC SG update (control plane).
//   - host-agent (here): enforcement at the data plane per TAP device.
//   - network-controller: IP allocation / VTEP; not a policy owner.
//
// Policy propagation path (async):
//   worker reads GetEffectiveSGRulesForNIC(nicID) from DB →
//   calls ProgramSGPolicy(instanceID, tapDevice, ingressRules, egressRules) →
//   host-agent programs nftables chains.
//
// Source: vm-14-02__blueprint__ §core_contracts "NIC-Centric Policy Model",
//         §implementation_decisions "per-direction chain naming".
//         vm-14-03__blueprint__ §core_contracts "Public Connectivity Contract".

import (
	"context"
	"fmt"
)

// ── SGPolicy types ────────────────────────────────────────────────────────────

// SGRuleIngress is an ingress security group rule for host-agent enforcement.
// Mirrors the API/DB shape; direction is implicit (ingress).
type SGRuleIngress struct {
	ID            string
	Protocol      string  // "tcp" | "udp" | "icmp" | "all"
	PortFrom      *int    // nil = any
	PortTo        *int    // nil = any
	SourceCIDR    *string // nil when matching a SG reference
	SourceSGChain *string // nftables chain name for cross-SG matching (future)
}

// SGRuleEgress is an egress security group rule for host-agent enforcement.
type SGRuleEgress struct {
	ID       string
	Protocol string // "tcp" | "udp" | "icmp" | "all"
	PortFrom *int
	PortTo   *int
	DestCIDR *string
}

// SGChainNames holds the canonical nftables chain names for a NIC.
// Chain names are deterministic so they can be referenced in nat/filter tables.
//
// Source: vm-14-02__blueprint__ §implementation_decisions
//
//	"chains must be named deterministically so that routing rules survive updates."
type SGChainNames struct {
	// IngressChain is the nftables chain holding ingress ALLOW rules for this NIC.
	// Name format: cpvm-sg-ingress-<first 12 chars of instanceID>
	IngressChain string
	// EgressChain is the nftables chain holding egress ALLOW rules for this NIC.
	EgressChain string
}

// GetSGChainNames returns the canonical nftables chain names for an instance NIC.
// These names are stable across policy updates — callers can jump to them from
// filter table hooks without knowing the full rule set.
func GetSGChainNames(instanceID string) SGChainNames {
	suffix := instanceID
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	return SGChainNames{
		IngressChain: "cpvm-sg-in-" + suffix,
		EgressChain:  "cpvm-sg-eg-" + suffix,
	}
}

// ProgramSGPolicy programs per-direction nftables chains for a NIC.
//
// Chain semantics (when fully implemented):
//  1. Flush the existing per-NIC ingress and egress chains.
//  2. Recreate both chains with an implicit DROP policy (deny-all baseline).
//  3. Add ACCEPT rules per ingressRules and egressRules slices respectively.
//  4. Ensure the filter table's INPUT and OUTPUT hooks jump to these chains
//     for traffic on the TAP device.
//
// This replaces ApplySGPolicy (which used a flat []SGRule slice with no direction
// separation). The old ApplySGPolicy remains for backward compatibility with
// existing callers during the transition; new callers must use ProgramSGPolicy.
//
// VM-P3A Job 2: Stub — logs chain names and rule counts, returns nil.
// Full nftables implementation is deferred; the seam is established so that
// swapping in the real implementation requires only the body of this function.
//
// Source: vm-14-02__blueprint__ §core_contracts "NIC-Centric Policy Model".
func (n *NetworkManager) ProgramSGPolicy(
	ctx context.Context,
	instanceID, tapDevice string,
	ingressRules []SGRuleIngress,
	egressRules []SGRuleEgress,
) error {
	chains := GetSGChainNames(instanceID)

	if n.dryRun {
		n.log.Warn("NETWORK_DRY_RUN=true: skipping SG policy programming",
			"instance_id", instanceID,
			"tap_device", tapDevice,
			"ingress_chain", chains.IngressChain,
			"egress_chain", chains.EgressChain,
			"ingress_rules", len(ingressRules),
			"egress_rules", len(egressRules),
		)
		return nil
	}

	// STUB: full nftables programming deferred.
	// When implemented:
	//   nft flush chain inet filter <IngressChain>
	//   nft flush chain inet filter <EgressChain>
	//   nft add chain inet filter <IngressChain> { type filter hook input priority 0; policy drop; }
	//   nft add chain inet filter <EgressChain>  { type filter hook output priority 0; policy drop; }
	//   for each ingressRule: nft add rule inet filter <IngressChain> <match> accept
	//   for each egressRule:  nft add rule inet filter <EgressChain>  <match> accept
	n.log.Warn("ProgramSGPolicy: nftables enforcement not yet implemented — stub only",
		"instance_id", instanceID,
		"tap_device", tapDevice,
		"ingress_chain", chains.IngressChain,
		"egress_chain", chains.EgressChain,
		"ingress_rules", len(ingressRules),
		"egress_rules", len(egressRules),
	)
	return nil
}

// RemoveSGPolicy is defined in network.go (the original VM-P2A-S3 stub).
// VM-P3A Job 2 upgrades its log message to use deterministic chain names by
// calling GetSGChainNames. The function signature is unchanged so callers are unaffected.
//
// Because RemoveSGPolicy is already defined on *NetworkManager in network.go,
// it is NOT redeclared here. The chain naming upgrade requires patching network.go
// directly — see the copy command for network.go in the delivery instructions.
//
// The upgraded implementation is documented here for reference:
//
//   func (n *NetworkManager) RemoveSGPolicy(_ context.Context, instanceID, tapDevice string) error {
//       chains := GetSGChainNames(instanceID)
//       n.log.Info("RemoveSGPolicy: flushing nftables chains (stub)",
//           "instance_id", instanceID,
//           "tap_device", tapDevice,
//           "ingress_chain", chains.IngressChain,
//           "egress_chain", chains.EgressChain,
//           "dry_run", n.dryRun,
//       )
//       // STUB: nft delete chain inet filter <IngressChain>
//       //       nft delete chain inet filter <EgressChain>
//       return nil
//   }

// ── Public IP state management ────────────────────────────────────────────────

// PublicIPState describes the desired public-connectivity state of an instance.
// Used by ApplyPublicIPState to diff against the currently programmed state
// and call ProgramNAT / RemoveNAT only when needed.
//
// Source: vm-14-03__blueprint__ §core_contracts "Public Connectivity Contract"
//
//	"NAT rules must be programmed atomically and must not be double-applied."
type PublicIPState struct {
	InstanceID string
	PrivateIP  string
	// PublicIP is the EIP associated to this instance's primary NIC.
	// Empty string means no public connectivity — RemoveNAT will be called.
	PublicIP string
}

// ApplyPublicIPState diffs the old and new PublicIPState and calls the minimal
// set of ProgramNAT / RemoveNAT operations to reach the desired state.
//
// Rules:
//   - If old and new PublicIP are identical: no-op.
//   - If new PublicIP is empty but old is not: calls RemoveNAT for old.
//   - If new PublicIP differs from old (including old being empty): calls
//     RemoveNAT for old (if non-empty) then ProgramNAT for new.
//
// This prevents duplicate iptables rules when a policy update is applied
// multiple times (idempotency by diff).
//
// Source: vm-14-03__blueprint__ §core_contracts "Public Connectivity Contract".
func (n *NetworkManager) ApplyPublicIPState(ctx context.Context, oldState, newState PublicIPState) error {
	if oldState.PublicIP == newState.PublicIP {
		return nil
	}

	instanceID := newState.InstanceID
	privateIP := newState.PrivateIP

	// Remove old NAT rules if old public IP was set.
	if oldState.PublicIP != "" {
		if err := n.RemoveNAT(ctx, instanceID, privateIP, oldState.PublicIP); err != nil {
			return fmt.Errorf("ApplyPublicIPState: remove old NAT: %w", err)
		}
	}

	// Program new NAT rules if new public IP is set.
	if newState.PublicIP != "" {
		if err := n.ProgramNAT(ctx, instanceID, privateIP, newState.PublicIP); err != nil {
			return fmt.Errorf("ApplyPublicIPState: program new NAT: %w", err)
		}
	}

	return nil
}
