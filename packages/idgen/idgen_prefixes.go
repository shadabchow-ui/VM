package idgen

// idgen_prefixes.go — M9 Slice 4: Additional ID prefixes for networking.
//
// This file adds the PrefixNIC constant for network interface IDs.
// Add this to your existing idgen package (packages/idgen/idgen.go).
//
// Source: P2_VPC_NETWORK_CONTRACT §5.2 (NIC ID format).

// ============================================================================
// INTEGRATION INSTRUCTIONS
// ============================================================================
//
// Add the following constant to your existing idgen package:
//
// const PrefixNIC = "nic_"
//
// The idgen.New function should already support any string prefix.
// ============================================================================

const PrefixNIC = "nic_"
