package main

// instance_handlers_networking.go — M9 Slice 4 networking helper functions.
//
// Isolated helpers called from instance_handlers.go for VPC networking.
// Does NOT redefine any existing handlers, auth, or error helpers.

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// M9 networking error codes (extend existing error codes in instance_errors.go)
const (
	errSubnetNotFound       = "subnet_not_found"
	errSubnetExhausted      = "subnet_exhausted"
	errInvalidSecurityGroup = "invalid_security_group"
)

// createInstanceNetworking creates VPC networking for a new instance.
// Called only when subnet_id is provided. Returns NIC or writes error and returns nil.
func (s *server) createInstanceNetworking(
	w http.ResponseWriter,
	r *http.Request,
	instanceID, principal string,
	cfg *NetworkingConfig,
) (*db.NetworkInterfaceRow, error) {
	ctx := r.Context()

	// Validate subnet exists
	subnet, err := s.repo.GetSubnetByID(ctx, cfg.SubnetID)
	if err != nil {
		s.log.Error("GetSubnetByID failed", "subnet_id", cfg.SubnetID, "error", err)
		writeInternalError(w)
		return nil, err
	}
	if subnet == nil || subnet.DeletedAt != nil {
		writeAPIError(w, http.StatusNotFound, errSubnetNotFound,
			fmt.Sprintf("Subnet '%s' does not exist.", cfg.SubnetID), "networking.subnet_id")
		return nil, fmt.Errorf("subnet not found")
	}

	// Validate VPC ownership
	vpc, err := s.repo.GetVPCByID(ctx, subnet.VPCID)
	if err != nil {
		s.log.Error("GetVPCByID failed", "vpc_id", subnet.VPCID, "error", err)
		writeInternalError(w)
		return nil, err
	}
	if vpc == nil || vpc.OwnerPrincipalID != principal {
		writeAPIError(w, http.StatusNotFound, errSubnetNotFound,
			fmt.Sprintf("Subnet '%s' does not exist.", cfg.SubnetID), "networking.subnet_id")
		return nil, fmt.Errorf("vpc not accessible")
	}

	// Resolve security groups
	sgIDs := cfg.SecurityGroupIDs

	// M9: Enforce SG limit (max 5 per NIC per P2_VPC_NETWORK_CONTRACT §4.7 SG-I-3)
	if len(sgIDs) > 5 {
		writeAPIErrors(w, []fieldErr{
			{
				target:  "security_group_ids",
				code:    errInvalidValue,
				message: "maximum 5 security groups per instance",
			},
		})
		return nil, fmt.Errorf("too many security groups")
	}

	if len(sgIDs) == 0 {
		if defaultSG, err := s.repo.GetDefaultSecurityGroupForVPC(ctx, vpc.ID); err == nil && defaultSG != nil {
			sgIDs = []string{defaultSG.ID}
		}
	} else {
		if err := s.repo.ValidateSecurityGroupsInVPC(ctx, sgIDs, vpc.ID, principal); err != nil {
			writeAPIError(w, http.StatusBadRequest, errInvalidSecurityGroup,
				err.Error(), "networking.security_group_ids")
			return nil, err
		}
	}

	// Allocate IP
	privateIP, err := s.repo.AllocateIPFromSubnet(ctx, cfg.SubnetID, instanceID)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, errSubnetExhausted,
			"No available IP addresses in the specified subnet.", "networking.subnet_id")
		return nil, err
	}

	// Create NIC
	nicID := idgen.New("nic")
	nic := &db.NetworkInterfaceRow{
		ID:         nicID,
		InstanceID: instanceID,
		SubnetID:   cfg.SubnetID,
		VPCID:      vpc.ID,
		PrivateIP:  privateIP,
		MACAddress: generateMACAddress(),
		IsPrimary:  true,
		Status:     "attaching",
	}

	if err := s.repo.CreateNetworkInterface(ctx, nic); err != nil {
		_ = s.repo.ReleaseIPFromSubnet(ctx, privateIP, cfg.SubnetID, instanceID)
		writeInternalError(w)
		return nil, err
	}

	// Link security groups
	for _, sgID := range sgIDs {
		_ = s.repo.CreateNICSecurityGroupLink(ctx, nicID, sgID)
	}

	return nic, nil
}

// enrichResponseWithNetworking adds networking info to InstanceResponse for VPC instances.
func (s *server) enrichResponseWithNetworking(ctx context.Context, resp *InstanceResponse, instanceID string) {
	nic, err := s.repo.GetPrimaryNetworkInterfaceByInstance(ctx, instanceID)
	if err != nil || nic == nil {
		return // Not a VPC instance
	}

	sgIDs, _ := s.repo.ListSecurityGroupIDsByNIC(ctx, nic.ID)
	if sgIDs == nil {
		sgIDs = []string{}
	}

	resp.Networking = &InstanceNetworkingResponse{
		VPCID:    nic.VPCID,
		SubnetID: nic.SubnetID,
		PrimaryInterface: &NetworkInterfaceResponse{
			ID:               nic.ID,
			PrivateIP:        nic.PrivateIP,
			MACAddress:       nic.MACAddress,
			Status:           nic.Status,
			SecurityGroupIDs: sgIDs,
		},
	}
	resp.PrivateIP = &nic.PrivateIP
	resp.PublicIP = nil
}

func generateMACAddress() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "02:00:00:00:00:01"
	}
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4])
}
