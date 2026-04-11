package main

// instance_validation_networking.go — M9 Slice 4 networking validation.
//
// Adds ONLY validateNetworkingConfig. Uses existing fieldErr type from
// instance_validation.go and existing error codes from instance_errors.go.

// validateNetworkingConfig validates the networking configuration.
// Returns nil if cfg is nil or SubnetID is empty (Phase 1 classic instance).
func validateNetworkingConfig(cfg *NetworkingConfig) []fieldErr {
	if cfg == nil || cfg.SubnetID == "" {
		return nil
	}

	var errs []fieldErr

	// Basic subnet ID validation
	if len(cfg.SubnetID) < 8 {
		errs = append(errs, fieldErr{
			target:  "subnet_id",
			code:    errInvalidValue,
			message: "subnet_id is not a valid subnet identifier",
		})
	}

	// Max 5 security groups per NIC (P2_VPC_NETWORK_CONTRACT §4.7 SG-I-3)
	if len(cfg.SecurityGroupIDs) > 5 {
		errs = append(errs, fieldErr{
			target:  "security_group_ids",
			code:    errInvalidValue,
			message: "maximum 5 security groups per instance",
		})
	}

	return errs
}
