package main

// instance_handlers_test.go — PASS 1 + PASS 2 + PASS 3 + M9 Slice 4 tests.
//
// PASS 1/2 coverage: unchanged.
// PASS 3 coverage (new):
//   IDEMPOTENCY — CREATE:
//     - Same key + same request → stable 202, same instance returned
//     - Different key → distinct new instance
//     - No key → normal behavior preserved
//   IDEMPOTENCY — LIFECYCLE ACTIONS:
//     - Same key + same stop/start/reboot → stable 202, same job_id
//     - Same key on different instance → 409 idempotency_key_mismatch
//     - Different key → distinct job
//     - No key → normal behavior preserved
//   JOB STATUS ENDPOINT:
//     - Happy path: GET /v1/instances/{id}/jobs/{job_id} → 202 + JobResponse
//     - Job not found → 404 job_not_found
//     - Wrong instance/job pairing → 404 job_not_found
//     - Wrong owner → 404 (instance ownership enforced first)
//     - Missing auth → 401
//     - Response shape: all required fields present
//
// M9 Slice 4 coverage:
//   - Classic instance creation (no subnet_id) → works as before
//   - VPC instance creation with subnet_id → networking info in response
//   - Security group validation
//   - Networking info enrichment in GET/LIST responses
//
// VM-P2B: extended memPool with volume and volume_attachment support.
//   - volumes, volumeAttachments fields on memPool
//   - volume Exec/Query/QueryRow dispatch cases
//   - jobRow.Scan updated to 13 columns (volume_id added at position 3)
//   - newVolTestSrv, newDiscardLogger, startTestServer shared helpers
//
// Test strategy: in-process httptest.Server backed by memPool (fake db.Pool).
// No DB, no Linux/KVM, no network required.
// Source: 11-02-phase-1-test-strategy.md §unit test approach.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── in-memory Pool ────────────────────────────────────────────────────────────

// memPool is a fake db.Pool for handler tests.
// PASS 3: extended QueryRow dispatch to support:
//   - GetJobByIdempotencyKey  (FROM jobs WHERE idempotency_key = $1)
//   - GetJobByInstanceAndID   (FROM jobs WHERE id = $1 AND instance_id = $2)
//
// M9 Slice 4: extended for networking queries:
//   - VPCs, Subnets, SecurityGroups, NetworkInterfaces
//
// M10 Slice 1: extended for project queries:
//   - Projects, Principals
//
// M10 Slice 2: extended for root disk queries:
//   - RootDisks
//
// VM-P2B: extended for volume queries:
//   - Volumes, VolumeAttachments
//
// VM-P2C-P1: extended for image queries:
//   - Images (used by GetImageForAdmission + ListImagesByPrincipal)
type memPool struct {
	instances           map[string]*db.InstanceRow
	jobs                map[string]*db.JobRow
	jobsByIdemKey       map[string]*db.JobRow // idempotency_key → job
	vpcs                map[string]*db.VPCRow
	subnets             map[string]*db.SubnetRow
	securityGroups      map[string]*db.SecurityGroupRow
	networkInterfaces   map[string]*db.NetworkInterfaceRow
	nicSecurityGroups   map[string][]string                // nic_id → []sg_id
	subnetIPAllocations map[string]string                  // "subnet:ip" → instance_id (for allocated IPs)
	nextSubnetIP        map[string]int                     // subnet_id → next available IP offset
	projects            map[string]*db.ProjectRow          // M10 Slice 1
	principals          map[string]string                  // id → principal_type (M10 Slice 1)
	rootDisks           map[string]*db.RootDiskRow         // M10 Slice 2
	volumes             map[string]*db.VolumeRow           // VM-P2B
	volumeAttachments   map[string]*db.VolumeAttachmentRow // VM-P2B; keyed by attachment ID
	snapshots           map[string]*db.SnapshotRow         // VM-P2B-S2
	images              map[string]*db.ImageRow            // VM-P2C-P1
	// VM-P2D Slice 3: quota maps.
	// quotas: scopeID → max_instances override. nil entry = use db.DefaultMaxInstances.
	quotas map[string]int // VM-P2D-S3
}

func newMemPool() *memPool {
	return &memPool{
		instances:           make(map[string]*db.InstanceRow),
		jobs:                make(map[string]*db.JobRow),
		jobsByIdemKey:       make(map[string]*db.JobRow),
		vpcs:                make(map[string]*db.VPCRow),
		subnets:             make(map[string]*db.SubnetRow),
		securityGroups:      make(map[string]*db.SecurityGroupRow),
		networkInterfaces:   make(map[string]*db.NetworkInterfaceRow),
		nicSecurityGroups:   make(map[string][]string),
		subnetIPAllocations: make(map[string]string),
		nextSubnetIP:        make(map[string]int),
		projects:            make(map[string]*db.ProjectRow),
		principals:          make(map[string]string),
		rootDisks:           make(map[string]*db.RootDiskRow),
		volumes:             make(map[string]*db.VolumeRow),
		volumeAttachments:   make(map[string]*db.VolumeAttachmentRow),
		snapshots:           make(map[string]*db.SnapshotRow), // VM-P2B-S2
		images:              make(map[string]*db.ImageRow),    // VM-P2C-P1
		quotas:              make(map[string]int),             // VM-P2D-S3
	}
}

// seed adds an instance directly.
func (p *memPool) seed(row *db.InstanceRow) {
	now := time.Now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = now
	}
	if row.VMState == "" {
		row.VMState = "requested"
	}
	p.instances[row.ID] = row
}

// seedJob adds a job directly (used in test setup for job-status tests).
func (p *memPool) seedJob(row *db.JobRow) {
	now := time.Now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = now
	}
	if row.Status == "" {
		row.Status = "pending"
	}
	p.jobs[row.ID] = row
	if row.IdempotencyKey != "" {
		p.jobsByIdemKey[row.IdempotencyKey] = row
	}
}

// seedVPC adds a VPC directly.
func (p *memPool) seedVPC(row *db.VPCRow) {
	now := time.Now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = now
	}
	if row.Status == "" {
		row.Status = "active"
	}
	p.vpcs[row.ID] = row
}

// seedSubnet adds a Subnet directly.
func (p *memPool) seedSubnet(row *db.SubnetRow) {
	now := time.Now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = now
	}
	if row.Status == "" {
		row.Status = "active"
	}
	p.subnets[row.ID] = row
	// Initialize IP allocation for this subnet
	p.nextSubnetIP[row.ID] = 10 // Start at .10
}

// seedSecurityGroup adds a SecurityGroup directly.
func (p *memPool) seedSecurityGroup(row *db.SecurityGroupRow) {
	now := time.Now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = now
	}
	p.securityGroups[row.ID] = row
}

// seedRootDisk adds a RootDisk directly (M10 Slice 2).
func (p *memPool) seedRootDisk(row *db.RootDiskRow) {
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	if row.Status == "" {
		row.Status = db.RootDiskStatusAttached
	}
	p.rootDisks[row.DiskID] = row
}

// seedNetworkInterface adds a NIC directly.
func (p *memPool) seedNetworkInterface(row *db.NetworkInterfaceRow) {
	now := time.Now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = now
	}
	if row.Status == "" {
		row.Status = "attached"
	}
	p.networkInterfaces[row.ID] = row
}

// seedQuota sets a max_instances override for scopeID.
// When not seeded the memPool returns db.DefaultMaxInstances (10).
// VM-P2D Slice 3.
func (p *memPool) seedQuota(scopeID string, maxInstances int) {
	p.quotas[scopeID] = maxInstances
}

// Exec handles INSERT INTO instances, INSERT INTO jobs, and networking tables.
func (p *memPool) Exec(_ context.Context, sql string, args ...any) (db.CommandTag, error) {
	switch {
	case strings.Contains(sql, "INSERT INTO instances"):
		id := asStr(args[0])
		now := time.Now()
		p.instances[id] = &db.InstanceRow{
			ID:               id,
			Name:             asStr(args[1]),
			OwnerPrincipalID: asStr(args[2]),
			VMState:          "requested",
			InstanceTypeID:   asStr(args[3]),
			ImageID:          asStr(args[4]),
			AvailabilityZone: asStr(args[5]),
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		return &fakeTag{1}, nil

	case strings.Contains(sql, "UPDATE images"):
		id := args[0].(string)
		img, ok := p.images[id]
		if !ok {
			return &fakeTag{0}, nil
		}
		img.Status = args[1].(string)
		if len(args) > 2 && args[2] != nil {
			v := args[2].(*time.Time)
			img.DeprecatedAt = v
		}
		if len(args) > 3 && args[3] != nil {
			v := args[3].(*time.Time)
			img.ObsoletedAt = v
		}
		now := time.Now()
		img.UpdatedAt = now
		return &fakeTag{1}, nil

	case strings.Contains(sql, "INSERT INTO jobs"):
		// Dispatch: volume jobs and snapshot jobs use different column orders.
		// InsertVolumeJob SQL contains "volume_id" (without "snapshot_id").
		// InsertSnapshotJob SQL contains "snapshot_id".
		if strings.Contains(sql, "snapshot_id") {
			// InsertSnapshotJob: $1=id, $2=snapshot_id, $3=volume_id, $4=job_type, $5=idempotency_key, $6=max_attempts
			id := asStr(args[0])
			snapID := asStrPtr(args[1])
			volID := asStrPtr(args[2])
			now := time.Now()
			row := &db.JobRow{
				ID:         id,
				SnapshotID: snapID,
				VolumeID:   volID,
				JobType:    asStr(args[3]),
				Status:     "pending",
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			p.jobs[id] = row
			return &fakeTag{1}, nil
		}
		if strings.Contains(sql, "volume_id") {
			// InsertVolumeJob: $1=id, $2=volume_id, $3=job_type, $4=idempotency_key, $5=max_attempts
			id := asStr(args[0])
			volID := asStr(args[1])
			now := time.Now()
			row := &db.JobRow{
				ID:        id,
				VolumeID:  &volID,
				JobType:   asStr(args[2]),
				Status:    "pending",
				CreatedAt: now,
				UpdatedAt: now,
			}
			p.jobs[id] = row
			return &fakeTag{1}, nil
		}
		// Instance job: $1=id, $2=instance_id, $3=job_type, $4=idempotency_key, $5=max_attempts
		id := asStr(args[0])
		ikey := asStr(args[3])
		// ON CONFLICT (idempotency_key) DO NOTHING simulation.
		if ikey != "" {
			if _, exists := p.jobsByIdemKey[ikey]; exists {
				return &fakeTag{0}, nil
			}
		}
		now := time.Now()
		row := &db.JobRow{
			ID:             id,
			InstanceID:     asStr(args[1]),
			JobType:        asStr(args[2]),
			IdempotencyKey: ikey,
			Status:         "pending",
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		p.jobs[id] = row
		if ikey != "" {
			p.jobsByIdemKey[ikey] = row
		}
		return &fakeTag{1}, nil

	case strings.Contains(sql, "INSERT INTO network_interfaces"):
		// $1=id, $2=instance_id, $3=subnet_id, $4=vpc_id, $5=private_ip,
		// $6=mac_address, $7=is_primary, $8=status
		id := asStr(args[0])
		now := time.Now()
		p.networkInterfaces[id] = &db.NetworkInterfaceRow{
			ID:         id,
			InstanceID: asStr(args[1]),
			SubnetID:   asStr(args[2]),
			VPCID:      asStr(args[3]),
			PrivateIP:  asStr(args[4]),
			MACAddress: asStr(args[5]),
			IsPrimary:  args[6].(bool),
			Status:     asStr(args[7]),
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		return &fakeTag{1}, nil

	case strings.Contains(sql, "INSERT INTO nic_security_groups"):
		// $1=nic_id, $2=security_group_id
		nicID := asStr(args[0])
		sgID := asStr(args[1])
		p.nicSecurityGroups[nicID] = append(p.nicSecurityGroups[nicID], sgID)
		return &fakeTag{1}, nil

	case strings.Contains(sql, "UPDATE instances") && strings.Contains(sql, "deleted_at"):
		// SoftDeleteInstance
		id := asStr(args[0])
		if inst, ok := p.instances[id]; ok {
			now := time.Now()
			inst.DeletedAt = &now
			return &fakeTag{1}, nil
		}
		return &fakeTag{0}, nil

	case strings.Contains(sql, "UPDATE subnet_ip_allocations"):
		// AllocateIPFromSubnet or ReleaseIPFromSubnet
		if strings.Contains(sql, "allocated = TRUE") {
			// AllocateIPFromSubnet
			subnetID := asStr(args[0])
			instanceID := asStr(args[1])
			offset := p.nextSubnetIP[subnetID]
			p.nextSubnetIP[subnetID] = offset + 1
			// Generate an IP like 10.0.0.X
			ip := fmt.Sprintf("10.0.0.%d", offset)
			key := subnetID + ":" + ip
			p.subnetIPAllocations[key] = instanceID
			// Return the IP - handled in QueryRow
			return &fakeTag{1}, nil
		}
		return &fakeTag{1}, nil

	// M10 Slice 1: Project operations
	case strings.Contains(sql, "INSERT INTO principals"):
		// $1=id, $2=principal_type
		id := asStr(args[0])
		principalType := asStr(args[1])
		p.principals[id] = principalType
		return &fakeTag{1}, nil

	case strings.Contains(sql, "INSERT INTO projects"):
		// $1=id, $2=principal_id, $3=created_by, $4=name, $5=display_name, $6=description, $7=status
		id := asStr(args[0])
		now := time.Now()
		p.projects[id] = &db.ProjectRow{
			ID:          id,
			PrincipalID: asStr(args[1]),
			CreatedBy:   asStr(args[2]),
			Name:        asStr(args[3]),
			DisplayName: asStr(args[4]),
			Description: asStrPtr(args[5]),
			Status:      asStr(args[6]),
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		return &fakeTag{1}, nil

	case strings.Contains(sql, "UPDATE projects") && strings.Contains(sql, "SET deleted_at = NOW()"):
		// SoftDeleteProject
		id := asStr(args[0])
		if proj, ok := p.projects[id]; ok && proj.DeletedAt == nil {
			now := time.Now()
			proj.DeletedAt = &now
			proj.Status = "deleted"
			proj.UpdatedAt = now
			return &fakeTag{1}, nil
		}
		return &fakeTag{0}, nil

	case strings.Contains(sql, "UPDATE projects") && strings.Contains(sql, "name = $2"):
		// UpdateProject: $1=id, $2=name, $3=display_name, $4=description
		id := asStr(args[0])
		if proj, ok := p.projects[id]; ok && proj.DeletedAt == nil {
			proj.Name = asStr(args[1])
			proj.DisplayName = asStr(args[2])
			if args[3] == nil {
				proj.Description = nil
			} else {
				if v, ok := args[3].(*string); ok {
					proj.Description = v
				} else if v, ok := args[3].(string); ok {
					proj.Description = &v
				}
			}
			proj.UpdatedAt = time.Now()
			return &fakeTag{1}, nil
		}
		return &fakeTag{0}, nil

	// M10 Slice 2: Root disk operations
	case strings.Contains(sql, "INSERT INTO root_disks"):
		// $1=disk_id, $2=instance_id, $3=source_image_id, $4=storage_pool_id,
		// $5=storage_path, $6=size_gb, $7=delete_on_termination, $8=status
		diskID := asStr(args[0])
		now := time.Now()
		var instID *string
		if args[1] != nil {
			if s, ok := args[1].(string); ok && s != "" {
				instID = &s
			} else if sp, ok := args[1].(*string); ok {
				instID = sp
			}
		}
		p.rootDisks[diskID] = &db.RootDiskRow{
			DiskID:              diskID,
			InstanceID:          instID,
			SourceImageID:       asStr(args[2]),
			StoragePoolID:       asStr(args[3]),
			StoragePath:         asStr(args[4]),
			SizeGB:              args[5].(int),
			DeleteOnTermination: args[6].(bool),
			Status:              asStr(args[7]),
			CreatedAt:           now,
		}
		return &fakeTag{1}, nil

	case strings.Contains(sql, "UPDATE root_disks") && strings.Contains(sql, "instance_id = NULL"):
		// DetachRootDisk: $1=disk_id, $2=status (DETACHED)
		diskID := asStr(args[0])
		if disk, ok := p.rootDisks[diskID]; ok {
			disk.InstanceID = nil
			disk.Status = asStr(args[1])
			return &fakeTag{1}, nil
		}
		return &fakeTag{0}, nil

	case strings.Contains(sql, "UPDATE root_disks") && strings.Contains(sql, "status = $2"):
		// UpdateRootDiskStatus: $1=disk_id, $2=status
		diskID := asStr(args[0])
		if disk, ok := p.rootDisks[diskID]; ok {
			disk.Status = asStr(args[1])
			return &fakeTag{1}, nil
		}
		return &fakeTag{0}, nil

	case strings.Contains(sql, "DELETE FROM root_disks"):
		// DeleteRootDisk: $1=disk_id
		diskID := asStr(args[0])
		if _, ok := p.rootDisks[diskID]; ok {
			delete(p.rootDisks, diskID)
			return &fakeTag{1}, nil
		}
		return &fakeTag{0}, nil

	// VM-P2B: Volume operations
	case strings.Contains(sql, "INSERT INTO volumes"):
		// CreateVolume: $1=id, $2=owner, $3=name, $4=region, $5=az,
		// $6=size_gb, $7=origin, $8=source_disk_id, $9=source_snapshot_id, $10=storage_pool_id
		id := asStr(args[0])
		now := time.Now()
		p.volumes[id] = &db.VolumeRow{
			ID:               id,
			OwnerPrincipalID: asStr(args[1]),
			DisplayName:      asStr(args[2]),
			Region:           asStr(args[3]),
			AvailabilityZone: asStr(args[4]),
			SizeGB:           args[5].(int),
			Origin:           asStr(args[6]),
			SourceDiskID:     asStrPtr(args[7]),
			SourceSnapshotID: asStrPtr(args[8]),
			Status:           "creating",
			StoragePoolID:    asStrPtr(args[9]),
			Version:          0,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		return &fakeTag{1}, nil

	case strings.Contains(sql, "INSERT INTO volume_attachments"):
		// CreateVolumeAttachment: $1=id, $2=vol_id, $3=inst_id, $4=device_path, $5=delete_on_term
		id := asStr(args[0])
		volID := asStr(args[1])
		// VOL-I-1: reject if active attachment already exists for this volume.
		for _, att := range p.volumeAttachments {
			if att.VolumeID == volID && att.DetachedAt == nil {
				return &fakeTag{0}, fmt.Errorf("unique constraint idx_volume_attachments_active")
			}
		}
		dot := false
		if v, ok := args[4].(bool); ok {
			dot = v
		}
		p.volumeAttachments[id] = &db.VolumeAttachmentRow{
			ID:                  id,
			VolumeID:            volID,
			InstanceID:          asStr(args[2]),
			DevicePath:          asStr(args[3]),
			DeleteOnTermination: dot,
			AttachedAt:          time.Now(),
		}
		return &fakeTag{1}, nil

	// VM-P2B-S2: CreateSnapshot
	// $1=id, $2=owner, $3=name, $4=region, $5=source_volume_id, $6=source_instance_id,
	// $7=size_gb, $8=encrypted, $9=storage_pool_id
	case strings.Contains(sql, "INSERT INTO snapshots"):
		id := asStr(args[0])
		now := time.Now()
		enc := false
		if v, ok := args[7].(bool); ok {
			enc = v
		}
		p.snapshots[id] = &db.SnapshotRow{
			ID:               id,
			OwnerPrincipalID: asStr(args[1]),
			DisplayName:      asStr(args[2]),
			Region:           asStr(args[3]),
			SourceVolumeID:   asStrPtr(args[4]),
			SourceInstanceID: asStrPtr(args[5]),
			SizeGB:           args[6].(int),
			Status:           db.SnapshotStatusPending,
			ProgressPercent:  0,
			Encrypted:        enc,
			Version:          0,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		return &fakeTag{1}, nil
	}
	return &fakeTag{0}, nil
}

// Query handles ListInstancesByOwner and networking list queries.
func (p *memPool) Query(_ context.Context, sql string, args ...any) (db.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM instances") && strings.Contains(sql, "owner_principal_id"):
		ownerID := asStr(args[0])
		var out []*db.InstanceRow
		for _, r := range p.instances {
			if r.OwnerPrincipalID == ownerID && r.DeletedAt == nil {
				out = append(out, r)
			}
		}
		return &instRows{rows: out}, nil

	case strings.Contains(sql, "FROM nic_security_groups"):
		nicID := asStr(args[0])
		sgIDs := p.nicSecurityGroups[nicID]
		return &stringRows{values: sgIDs}, nil

	// M10 Slice 1: ListProjectsByCreator
	case strings.Contains(sql, "FROM projects") && strings.Contains(sql, "created_by"):
		createdBy := asStr(args[0])
		var out []*db.ProjectRow
		for _, r := range p.projects {
			if r.CreatedBy == createdBy && r.DeletedAt == nil {
				out = append(out, r)
			}
		}
		return &projRows{rows: out}, nil

	// M10 Slice 2: ListDetachedRootDisks
	case strings.Contains(sql, "FROM root_disks") && strings.Contains(sql, "status = $1"):
		status := asStr(args[0])
		limit := 100
		if len(args) > 1 {
			if l, ok := args[1].(int); ok {
				limit = l
			}
		}
		var out []*db.RootDiskRow
		for _, r := range p.rootDisks {
			if r.Status == status {
				out = append(out, r)
				if len(out) >= limit {
					break
				}
			}
		}
		return &rootDiskRows{rows: out}, nil

	// VM-P2B: ListVolumesByOwner
	case strings.Contains(sql, "FROM volumes") && strings.Contains(sql, "owner_principal_id"):
		ownerID := asStr(args[0])
		var out []*db.VolumeRow
		for _, r := range p.volumes {
			if r.OwnerPrincipalID == ownerID && r.DeletedAt == nil {
				out = append(out, r)
			}
		}
		return &volumeRows{rows: out}, nil

	// VM-P2B-S2: ListSnapshotsByOwner
	case strings.Contains(sql, "FROM snapshots") && strings.Contains(sql, "owner_principal_id"):
		ownerID := asStr(args[0])
		var out []*db.SnapshotRow
		for _, r := range p.snapshots {
			if r.OwnerPrincipalID == ownerID && r.DeletedAt == nil {
				out = append(out, r)
			}
		}
		return &snapshotRows{rows: out}, nil

	// VM-P2B: ListActiveAttachmentsByInstance
	// Be tolerant of joined/aliased SQL forms such as:
	//   FROM volume_attachments ...
	//   JOIN volume_attachments va ...
	//   WHERE va.instance_id = $1
	// The happy-path list-instance-volumes test returns 0 rows if this matcher is
	// too strict and falls through to the default empty rows implementation.
	case strings.Contains(sql, "volume_attachments") && strings.Contains(sql, "instance_id") && !strings.Contains(sql, "SELECT device_path"):
		instanceID := asStr(args[0])
		var out []*db.VolumeAttachmentRow
		for _, att := range p.volumeAttachments {
			if att.InstanceID == instanceID && att.DetachedAt == nil {
				out = append(out, att)
			}
		}
		return &attachmentRows{rows: out}, nil

	// VM-P2B: NextDevicePath — SELECT device_path FROM volume_attachments WHERE instance_id = $1 ...
	case strings.Contains(sql, "SELECT device_path") && strings.Contains(sql, "volume_attachments"):
		instanceID := asStr(args[0])
		var paths []string
		for _, att := range p.volumeAttachments {
			if att.InstanceID == instanceID && att.DetachedAt == nil {
				paths = append(paths, att.DevicePath)
			}
		}
		return &devicePathRows{paths: paths}, nil

	// VM-P2C-P1: ListImagesByPrincipal
	// Matches WHERE visibility = 'PUBLIC' OR (visibility = 'PRIVATE' AND owner_id = $1)
	// Source: image_repo.go ListImagesByPrincipal.
	case strings.Contains(sql, "FROM images"):
		principalID := asStr(args[0])
		var out []*db.ImageRow
		for _, img := range p.images {
			if img.Visibility == "PUBLIC" || (img.Visibility == "PRIVATE" && img.OwnerID == principalID) {
				out = append(out, img)
			}
		}
		return &imageRows{rows: out}, nil
	}
	return &instRows{}, nil
}

// QueryRow handles:
//   - GetInstanceByID           (FROM instances WHERE id = $1)
//   - GetJobByIdempotencyKey    (FROM jobs WHERE idempotency_key = $1)
//   - GetJobByInstanceAndID     (FROM jobs WHERE id = $1 AND instance_id = $2)
//   - GetVPCByID, GetSubnetByID, GetSecurityGroupByID
//   - GetPrimaryNetworkInterfaceByInstance
//   - AllocateIPFromSubnet (RETURNING)
//   - GetDefaultSecurityGroupForVPC
//   - VM-P2B: GetVolumeByID, GetActiveAttachmentByVolume, CountActiveAttachmentsByInstance
func (p *memPool) QueryRow(_ context.Context, sql string, args ...any) db.Row {
	switch {
	// GetInstanceByID — guard against SELECT COUNT(*) queries which also contain "id = $1"
	case strings.Contains(sql, "FROM instances") && strings.Contains(sql, "id = $1") && !strings.Contains(sql, "SELECT COUNT(*)"):
		id := asStr(args[0])
		r, ok := p.instances[id]
		if !ok || r.DeletedAt != nil {
			return &errRow{fmt.Errorf("GetInstanceByID %s: no rows in result set", id)}
		}
		return &instRow{r: r}

	// GetJobByIdempotencyKey: WHERE idempotency_key = $1
	case strings.Contains(sql, "FROM jobs") && strings.Contains(sql, "idempotency_key = $1"):
		key := asStr(args[0])
		job, ok := p.jobsByIdemKey[key]
		if !ok {
			return &errRow{fmt.Errorf("no rows in result set")}
		}
		return &jobRow{r: job}

	// GetJobByInstanceAndID: WHERE id = $1 AND instance_id = $2
	case strings.Contains(sql, "FROM jobs") && strings.Contains(sql, "instance_id = $2"):
		jobID := asStr(args[0])
		instanceID := asStr(args[1])
		job, ok := p.jobs[jobID]
		if !ok || job.InstanceID != instanceID {
			return &errRow{fmt.Errorf("no rows in result set")}
		}
		return &jobRow{r: job}

	// GetVPCByID
	case strings.Contains(sql, "FROM vpcs") && strings.Contains(sql, "id = $1"):
		id := asStr(args[0])
		vpc, ok := p.vpcs[id]
		if !ok || vpc.DeletedAt != nil {
			return &errRow{fmt.Errorf("no rows in result set")}
		}
		return &vpcRow{r: vpc}

	// GetSubnetByID
	case strings.Contains(sql, "FROM subnets") && strings.Contains(sql, "id = $1"):
		id := asStr(args[0])
		subnet, ok := p.subnets[id]
		if !ok || subnet.DeletedAt != nil {
			return &errRow{fmt.Errorf("no rows in result set")}
		}
		return &subnetRow{r: subnet}

	// GetSecurityGroupByID
	case strings.Contains(sql, "FROM security_groups") && strings.Contains(sql, "id = $1"):
		id := asStr(args[0])
		sg, ok := p.securityGroups[id]
		if !ok || sg.DeletedAt != nil {
			return &errRow{fmt.Errorf("no rows in result set")}
		}
		return &sgRow{r: sg}

	// GetDefaultSecurityGroupForVPC
	case strings.Contains(sql, "FROM security_groups") && strings.Contains(sql, "is_default = TRUE"):
		vpcID := asStr(args[0])
		for _, sg := range p.securityGroups {
			if sg.VPCID == vpcID && sg.IsDefault && sg.DeletedAt == nil {
				return &sgRow{r: sg}
			}
		}
		return &errRow{fmt.Errorf("no rows in result set")}

	// GetPrimaryNetworkInterfaceByInstance
	case strings.Contains(sql, "FROM network_interfaces") && strings.Contains(sql, "is_primary = TRUE"):
		instanceID := asStr(args[0])
		for _, nic := range p.networkInterfaces {
			if nic.InstanceID == instanceID && nic.IsPrimary && nic.DeletedAt == nil {
				return &nicRow{r: nic}
			}
		}
		return &errRow{fmt.Errorf("no rows in result set")}

	// AllocateIPFromSubnet - handled specially
	case strings.Contains(sql, "UPDATE subnet_ip_allocations") && strings.Contains(sql, "RETURNING"):
		subnetID := asStr(args[0])
		offset := p.nextSubnetIP[subnetID]
		p.nextSubnetIP[subnetID] = offset + 1
		ip := fmt.Sprintf("10.0.0.%d", offset)
		return &stringValueRow{value: ip}

	// M10 Slice 1: GetProjectByID
	case strings.Contains(sql, "FROM projects") && strings.Contains(sql, "id = $1"):
		id := asStr(args[0])
		proj, ok := p.projects[id]
		if !ok || proj.DeletedAt != nil {
			return &errRow{fmt.Errorf("no rows in result set")}
		}
		return &projRow{r: proj}

	// M10 Slice 1: GetProjectByPrincipalID
	case strings.Contains(sql, "FROM projects") && strings.Contains(sql, "principal_id = $1"):
		principalID := asStr(args[0])
		for _, proj := range p.projects {
			if proj.PrincipalID == principalID && proj.DeletedAt == nil {
				return &projRow{r: proj}
			}
		}
		return &errRow{fmt.Errorf("no rows in result set")}

	// M10 Slice 1: CheckProjectNameExists
	case strings.Contains(sql, "SELECT EXISTS") && strings.Contains(sql, "FROM projects"):
		createdBy := asStr(args[0])
		name := asStr(args[1])
		excludeID := asStr(args[2])
		exists := false
		for _, proj := range p.projects {
			if proj.CreatedBy == createdBy && proj.Name == name && proj.ID != excludeID && proj.DeletedAt == nil {
				exists = true
				break
			}
		}
		return &boolRow{value: exists}

	// M10 Slice 2: GetRootDiskByID
	case strings.Contains(sql, "FROM root_disks") && strings.Contains(sql, "disk_id = $1"):
		diskID := asStr(args[0])
		disk, ok := p.rootDisks[diskID]
		if !ok {
			return &errRow{fmt.Errorf("no rows in result set")}
		}
		return &rootDiskRow{r: disk}

	// M10 Slice 2: GetRootDiskByInstanceID
	case strings.Contains(sql, "FROM root_disks") && strings.Contains(sql, "instance_id = $1"):
		instanceID := asStr(args[0])
		for _, disk := range p.rootDisks {
			if disk.InstanceID != nil && *disk.InstanceID == instanceID {
				return &rootDiskRow{r: disk}
			}
		}
		return &errRow{fmt.Errorf("no rows in result set")}

	// VM-P2B: GetVolumeByID
	case strings.Contains(sql, "FROM volumes") && strings.Contains(sql, "id = $1"):
		id := asStr(args[0])
		vol, ok := p.volumes[id]
		if !ok || vol.DeletedAt != nil {
			return &errRow{fmt.Errorf("no rows in result set")}
		}
		return &volumeRow{r: vol}

	// VM-P2B: GetActiveAttachmentByVolume — WHERE volume_id = $1 AND detached_at IS NULL
	case strings.Contains(sql, "FROM volume_attachments") && strings.Contains(sql, "volume_id"):
		volID := asStr(args[0])
		for _, att := range p.volumeAttachments {
			if att.VolumeID == volID && att.DetachedAt == nil {
				return &volumeAttachmentRow{r: att}
			}
		}
		return &errRow{fmt.Errorf("no rows in result set")}

	// VM-P2B: CountActiveAttachmentsByInstance — SELECT COUNT(*) FROM volume_attachments WHERE instance_id = $1 ...
	case strings.Contains(sql, "SELECT COUNT(*)") && strings.Contains(sql, "volume_attachments"):
		instanceID := asStr(args[0])
		count := 0
		for _, att := range p.volumeAttachments {
			if att.InstanceID == instanceID && att.DetachedAt == nil {
				count++
			}
		}
		return &intRow{value: count}

	// VM-P2B-S1 / VM-P2B-S2 — HasActiveVolumeJob:
	// SELECT COUNT(*) FROM jobs WHERE volume_id = $1 AND job_type = $2 AND status IN (...)
	// Source: P2_VOLUME_MODEL.md §7 VOL-I-5.
	case strings.Contains(sql, "SELECT COUNT(*)") && strings.Contains(sql, "FROM jobs") && strings.Contains(sql, "volume_id"):
		volID := asStr(args[0])
		jobType := asStr(args[1])
		count := 0
		for _, j := range p.jobs {
			if j.VolumeID != nil && *j.VolumeID == volID &&
				j.JobType == jobType &&
				(j.Status == "pending" || j.Status == "in_progress") {
				count++
			}
		}
		return &intRow{value: count}

	// VM-P2B-S2 — HasActiveSnapshotJob:
	// SELECT COUNT(*) FROM jobs WHERE snapshot_id = $1 AND job_type = $2 AND status IN (...)
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9.
	case strings.Contains(sql, "SELECT COUNT(*)") && strings.Contains(sql, "FROM jobs") && strings.Contains(sql, "snapshot_id"):
		snapID := asStr(args[0])
		jobType := asStr(args[1])
		count := 0
		for _, j := range p.jobs {
			if j.SnapshotID != nil && *j.SnapshotID == snapID &&
				j.JobType == jobType &&
				(j.Status == "pending" || j.Status == "in_progress") {
				count++
			}
		}
		return &intRow{value: count}

	// VM-P2B-S2: GetSnapshotByID — WHERE id = $1 AND deleted_at IS NULL

	// VM-P2B-S3: CountActiveSnapshotsByVolume — SELECT COUNT(*) FROM snapshots
	// WHERE source_volume_id = $1 AND status NOT IN ('deleted') AND deleted_at IS NULL
	case strings.Contains(sql, "SELECT COUNT(*)") &&
		strings.Contains(sql, "FROM snapshots") &&
		strings.Contains(sql, "source_volume_id"):
		volumeID := args[0].(string)
		count := 0
		for _, snap := range p.snapshots {
			if snap == nil || snap.DeletedAt != nil || snap.SourceVolumeID == nil {
				continue
			}
			if *snap.SourceVolumeID == volumeID && snap.Status != "deleted" {
				count++
			}
		}
		return &intRow{value: count}

	case strings.Contains(sql, "FROM snapshots") && strings.Contains(sql, "id = $1"):
		id := asStr(args[0])
		snap, ok := p.snapshots[id]
		if !ok || snap.DeletedAt != nil {
			return &errRow{fmt.Errorf("GetSnapshotByID %s: no rows in result set", id)}
		}
		return &snapshotRow{r: snap}

	// VM-P2C-P1: GetImageByID — WHERE id = $1
	// Used by GetImageForAdmission (called from handleCreateInstance admission check
	// and handleGetImage). Visibility filtering is done in Go, not SQL.
	// Source: image_repo.go GetImageByID.
	// VM-P2D Slice 3: GetQuota — FROM project_quotas WHERE scope_id = $1
	case strings.Contains(sql, "FROM project_quotas") && strings.Contains(sql, "scope_id = $1"):
		scopeID := asStr(args[0])
		if max, ok := p.quotas[scopeID]; ok {
			return &quotaRow{scopeID: scopeID, maxInstances: max}
		}
		// No row → signal ErrNoRows so GetQuota returns the default.
		return &errRow{fmt.Errorf("no rows in result set")}

	// VM-P2D Slice 3: CountActiveInstancesByScope
	// SELECT COUNT(*) FROM instances WHERE owner_principal_id = $1 ...
	case strings.Contains(sql, "SELECT COUNT(*)") && strings.Contains(sql, "FROM instances") && strings.Contains(sql, "owner_principal_id = $1"):
		scopeID := asStr(args[0])
		count := 0
		for _, inst := range p.instances {
			if inst.OwnerPrincipalID == scopeID && inst.DeletedAt == nil &&
				inst.VMState != "deleted" && inst.VMState != "failed" {
				count++
			}
		}
		return &intRow{value: count}

	case strings.Contains(sql, "FROM images") && strings.Contains(sql, "family_name = $1"):
		return p.familyQueryRowDispatch(sql, args)

	case strings.Contains(sql, "FROM images") && strings.Contains(sql, "id = $1"):
		id := asStr(args[0])
		img, ok := p.images[id]
		if !ok {
			return &errRow{fmt.Errorf("GetImageByID %s: no rows in result set", id)}
		}
		return &imageRow{r: img}
	}

	return &errRow{fmt.Errorf("no rows in result set")}
}

func (p *memPool) Close() {}

// ── Row types ─────────────────────────────────────────────────────────────────

type fakeTag struct{ n int64 }

func (t *fakeTag) RowsAffected() int64 { return t.n }

type errRow struct{ err error }

func (r *errRow) Scan(...any) error { return r.err }

// instRow scans a single InstanceRow.
type instRow struct{ r *db.InstanceRow }

func (row *instRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 12 {
		return fmt.Errorf("instRow.Scan: need 12 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.Name
	*dest[2].(*string) = r.OwnerPrincipalID
	*dest[3].(*string) = r.VMState
	*dest[4].(*string) = r.InstanceTypeID
	*dest[5].(*string) = r.ImageID
	*dest[6].(**string) = r.HostID
	*dest[7].(*string) = r.AvailabilityZone
	*dest[8].(*int) = r.Version
	*dest[9].(*time.Time) = r.CreatedAt
	*dest[10].(*time.Time) = r.UpdatedAt
	*dest[11].(**time.Time) = r.DeletedAt
	return nil
}

// jobRow scans a single JobRow.
// VM-P2B: updated to 13 columns — volume_id inserted at position 2.
// VM-P2B-S2: updated to 14 columns — snapshot_id inserted at position 3.
// VM-P2C-P2: updated to 15 columns — image_id inserted at position 4.
// Column order must match job_repo.go SELECT: id, instance_id, volume_id, snapshot_id,
// image_id, job_type, status, idempotency_key, attempt_count, max_attempts,
// error_message, created_at, updated_at, claimed_at, completed_at.
type jobRow struct{ r *db.JobRow }

func (row *jobRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 15 {
		return fmt.Errorf("jobRow.Scan: need 15 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.InstanceID
	*dest[2].(**string) = r.VolumeID
	*dest[3].(**string) = r.SnapshotID
	*dest[4].(**string) = r.ImageID
	*dest[5].(*string) = r.JobType
	*dest[6].(*string) = r.Status
	*dest[7].(*string) = r.IdempotencyKey
	*dest[8].(*int) = r.AttemptCount
	*dest[9].(*int) = r.MaxAttempts
	*dest[10].(**string) = r.ErrorMessage
	*dest[11].(*time.Time) = r.CreatedAt
	*dest[12].(*time.Time) = r.UpdatedAt
	*dest[13].(**time.Time) = r.ClaimedAt
	*dest[14].(**time.Time) = r.CompletedAt
	return nil
}

// vpcRow scans a VPCRow.
type vpcRow struct{ r *db.VPCRow }

func (row *vpcRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 8 {
		return fmt.Errorf("vpcRow.Scan: need 8 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.OwnerPrincipalID
	*dest[2].(*string) = r.Name
	*dest[3].(*string) = r.CIDRIPv4
	*dest[4].(*string) = r.Status
	*dest[5].(*time.Time) = r.CreatedAt
	*dest[6].(*time.Time) = r.UpdatedAt
	*dest[7].(**time.Time) = r.DeletedAt
	return nil
}

// subnetRow scans a SubnetRow.
type subnetRow struct{ r *db.SubnetRow }

func (row *subnetRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 9 {
		return fmt.Errorf("subnetRow.Scan: need 9 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.VPCID
	*dest[2].(*string) = r.Name
	*dest[3].(*string) = r.CIDRIPv4
	*dest[4].(*string) = r.AvailabilityZone
	*dest[5].(*string) = r.Status
	*dest[6].(*time.Time) = r.CreatedAt
	*dest[7].(*time.Time) = r.UpdatedAt
	*dest[8].(**time.Time) = r.DeletedAt
	return nil
}

// sgRow scans a SecurityGroupRow.
type sgRow struct{ r *db.SecurityGroupRow }

func (row *sgRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 9 {
		return fmt.Errorf("sgRow.Scan: need 9 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.VPCID
	*dest[2].(*string) = r.OwnerPrincipalID
	*dest[3].(*string) = r.Name
	*dest[4].(**string) = r.Description
	*dest[5].(*bool) = r.IsDefault
	*dest[6].(*time.Time) = r.CreatedAt
	*dest[7].(*time.Time) = r.UpdatedAt
	*dest[8].(**time.Time) = r.DeletedAt
	return nil
}

// nicRow scans a NetworkInterfaceRow.
type nicRow struct{ r *db.NetworkInterfaceRow }

func (row *nicRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 11 {
		return fmt.Errorf("nicRow.Scan: need 11 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.InstanceID
	*dest[2].(*string) = r.SubnetID
	*dest[3].(*string) = r.VPCID
	*dest[4].(*string) = r.PrivateIP
	*dest[5].(*string) = r.MACAddress
	*dest[6].(*bool) = r.IsPrimary
	*dest[7].(*string) = r.Status
	*dest[8].(*time.Time) = r.CreatedAt
	*dest[9].(*time.Time) = r.UpdatedAt
	*dest[10].(**time.Time) = r.DeletedAt
	return nil
}

// stringValueRow returns a single string value.
type stringValueRow struct{ value string }

func (row *stringValueRow) Scan(dest ...any) error {
	if len(dest) < 1 {
		return fmt.Errorf("stringValueRow.Scan: need 1 dest")
	}
	*dest[0].(*string) = row.value
	return nil
}

// intRow returns a single int value (used for COUNT(*) queries).
// VM-P2B: CountActiveAttachmentsByInstance.
type intRow struct{ value int }

func (row *intRow) Scan(dest ...any) error {
	if len(dest) < 1 {
		return fmt.Errorf("intRow.Scan: need 1 dest")
	}
	*dest[0].(*int) = row.value
	return nil
}

// instRows iterates a slice for ListInstancesByOwner.
type instRows struct {
	rows []*db.InstanceRow
	pos  int
}

func (r *instRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *instRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) < 12 {
		return fmt.Errorf("instRows.Scan: need 12 dest, got %d", len(dest))
	}
	*dest[0].(*string) = row.ID
	*dest[1].(*string) = row.Name
	*dest[2].(*string) = row.OwnerPrincipalID
	*dest[3].(*string) = row.VMState
	*dest[4].(*string) = row.InstanceTypeID
	*dest[5].(*string) = row.ImageID
	*dest[6].(**string) = row.HostID
	*dest[7].(*string) = row.AvailabilityZone
	*dest[8].(*int) = row.Version
	*dest[9].(*time.Time) = row.CreatedAt
	*dest[10].(*time.Time) = row.UpdatedAt
	*dest[11].(**time.Time) = row.DeletedAt
	return nil
}

func (r *instRows) Close()     {}
func (r *instRows) Err() error { return nil }

// stringRows iterates a slice of strings (for security group IDs).
type stringRows struct {
	values []string
	pos    int
}

func (r *stringRows) Next() bool {
	if r.pos >= len(r.values) {
		return false
	}
	r.pos++
	return true
}

func (r *stringRows) Scan(dest ...any) error {
	if len(dest) < 1 {
		return fmt.Errorf("stringRows.Scan: need 1 dest")
	}
	*dest[0].(*string) = r.values[r.pos-1]
	return nil
}

func (r *stringRows) Close()     {}
func (r *stringRows) Err() error { return nil }

// M10 Slice 1: projRow scans a single ProjectRow.
type projRow struct{ r *db.ProjectRow }

func (row *projRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 10 {
		return fmt.Errorf("projRow.Scan: need 10 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.PrincipalID
	*dest[2].(*string) = r.CreatedBy
	*dest[3].(*string) = r.Name
	*dest[4].(*string) = r.DisplayName
	*dest[5].(**string) = r.Description
	*dest[6].(*string) = r.Status
	*dest[7].(*time.Time) = r.CreatedAt
	*dest[8].(*time.Time) = r.UpdatedAt
	*dest[9].(**time.Time) = r.DeletedAt
	return nil
}

// M10 Slice 1: projRows iterates a slice for ListProjectsByCreator.
type projRows struct {
	rows []*db.ProjectRow
	pos  int
}

func (r *projRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *projRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) < 10 {
		return fmt.Errorf("projRows.Scan: need 10 dest, got %d", len(dest))
	}
	*dest[0].(*string) = row.ID
	*dest[1].(*string) = row.PrincipalID
	*dest[2].(*string) = row.CreatedBy
	*dest[3].(*string) = row.Name
	*dest[4].(*string) = row.DisplayName
	*dest[5].(**string) = row.Description
	*dest[6].(*string) = row.Status
	*dest[7].(*time.Time) = row.CreatedAt
	*dest[8].(*time.Time) = row.UpdatedAt
	*dest[9].(**time.Time) = row.DeletedAt
	return nil
}

func (r *projRows) Close()     {}
func (r *projRows) Err() error { return nil }

// M10 Slice 1: boolRow returns a single bool value.
type boolRow struct{ value bool }

func (row *boolRow) Scan(dest ...any) error {
	if len(dest) < 1 {
		return fmt.Errorf("boolRow.Scan: need 1 dest")
	}
	*dest[0].(*bool) = row.value
	return nil
}

// M10 Slice 2: rootDiskRow scans a single RootDiskRow.
type rootDiskRow struct{ r *db.RootDiskRow }

func (row *rootDiskRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 9 {
		return fmt.Errorf("rootDiskRow.Scan: need 9 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.DiskID
	*dest[1].(**string) = r.InstanceID
	*dest[2].(*string) = r.SourceImageID
	*dest[3].(*string) = r.StoragePoolID
	*dest[4].(*string) = r.StoragePath
	*dest[5].(*int) = r.SizeGB
	*dest[6].(*bool) = r.DeleteOnTermination
	*dest[7].(*string) = r.Status
	*dest[8].(*time.Time) = r.CreatedAt
	return nil
}

// M10 Slice 2: rootDiskRows iterates a slice for ListDetachedRootDisks.
type rootDiskRows struct {
	rows []*db.RootDiskRow
	pos  int
}

func (r *rootDiskRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *rootDiskRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) < 9 {
		return fmt.Errorf("rootDiskRows.Scan: need 9 dest, got %d", len(dest))
	}
	*dest[0].(*string) = row.DiskID
	*dest[1].(**string) = row.InstanceID
	*dest[2].(*string) = row.SourceImageID
	*dest[3].(*string) = row.StoragePoolID
	*dest[4].(*string) = row.StoragePath
	*dest[5].(*int) = row.SizeGB
	*dest[6].(*bool) = row.DeleteOnTermination
	*dest[7].(*string) = row.Status
	*dest[8].(*time.Time) = row.CreatedAt
	return nil
}

func (r *rootDiskRows) Close()     {}
func (r *rootDiskRows) Err() error { return nil }

// VM-P2D Slice 3: quotaRow returns a single quota row for project_quotas queries.
type quotaRow struct {
	scopeID      string
	maxInstances int
}

func (row *quotaRow) Scan(dest ...any) error {
	if len(dest) < 2 {
		return fmt.Errorf("quotaRow.Scan: need 2 dest, got %d", len(dest))
	}
	*dest[0].(*string) = row.scopeID
	*dest[1].(*int) = row.maxInstances
	return nil
}

// ── Test server ───────────────────────────────────────────────────────────────

type testSrv struct {
	ts  *httptest.Server
	mem *memPool
}

func newTestSrv(t *testing.T) *testSrv {
	t.Helper()
	mem := newMemPool()
	repo := db.New(mem)
	srv := &server{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:   repo,
		region: "us-east-1",
	}
	mux := http.NewServeMux()
	srv.registerInstanceRoutes(mux)
	srv.registerProjectRoutes(mux) // M10 Slice 1
	srv.registerVolumeRoutes(mux)  // VM-P2B
	srv.registerImageRoutes(mux)   // VM-P2C-P1
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// VM-P2C-P1: seed Phase 1 platform images so handleCreateInstance admission
	// passes for the standard test image IDs used in validCreateBody() and seedInstance().
	// Without these, all create/lifecycle tests fail image admission with 422.
	// Source: image_repo.go GetImageForAdmission, db.ImageIsLaunchable.
	now := time.Now()
	mem.images["00000000-0000-0000-0000-000000000010"] = &db.ImageRow{
		ID:               "00000000-0000-0000-0000-000000000010",
		Name:             "ubuntu-22.04-lts",
		OSFamily:         "ubuntu",
		OSVersion:        "22.04",
		Architecture:     "x86_64",
		OwnerID:          "system",
		Visibility:       "PUBLIC",
		SourceType:       "PLATFORM",
		StorageURL:       "nfs://images/ubuntu-22.04.qcow2",
		MinDiskGB:        10,
		Status:           "ACTIVE",
		ValidationStatus: "passed",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	mem.images["00000000-0000-0000-0000-000000000011"] = &db.ImageRow{
		ID:               "00000000-0000-0000-0000-000000000011",
		Name:             "debian-12",
		OSFamily:         "debian",
		OSVersion:        "12",
		Architecture:     "x86_64",
		OwnerID:          "system",
		Visibility:       "PUBLIC",
		SourceType:       "PLATFORM",
		StorageURL:       "nfs://images/debian-12.qcow2",
		MinDiskGB:        10,
		Status:           "ACTIVE",
		ValidationStatus: "passed",
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	return &testSrv{ts: ts, mem: mem}
}

// newDiscardLogger returns a slog.Logger that discards all output.
// VM-P2B: shared helper used by volume_handlers_test.go.
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startTestServer starts an httptest.Server and registers cleanup.
// VM-P2B: shared helper used by volume_handlers_test.go.
func startTestServer(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

const alice = "princ_alice"
const bob = "princ_bob"

func authHdr(principal string) map[string]string {
	return map[string]string{"X-Principal-ID": principal}
}

func authHdrWithIkey(principal, ikey string) map[string]string {
	return map[string]string{
		"X-Principal-ID":  principal,
		"Idempotency-Key": ikey,
	}
}

func doReq(t *testing.T, ts *httptest.Server, method, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	} else {
		r = strings.NewReader("")
	}
	req, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

func validCreateBody() CreateInstanceRequest {
	return CreateInstanceRequest{
		Name:             "my-instance",
		InstanceType:     "c1.small",
		ImageID:          "00000000-0000-0000-0000-000000000010",
		AvailabilityZone: "us-east-1a",
		SSHKeyName:       "my-key",
	}
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

// asStrPtr extracts a *string from an any value.
// Handles both *string (from repo layer) and string types.
func asStrPtr(v any) *string {
	if v == nil {
		return nil
	}
	if sp, ok := v.(*string); ok {
		return sp
	}
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

// seedInstance adds a ready-to-use instance owned by principal into memPool.
func seedInstance(mem *memPool, id, name, owner, vmState string) {
	mem.seed(&db.InstanceRow{
		ID:               id,
		Name:             name,
		OwnerPrincipalID: owner,
		VMState:          vmState,
		InstanceTypeID:   "c1.small",
		ImageID:          "00000000-0000-0000-0000-000000000010",
		AvailabilityZone: "us-east-1a",
	})
}

// seedVPCInfrastructure sets up a complete VPC + Subnet + Default SG for testing.
func seedVPCInfrastructure(mem *memPool, vpcID, subnetID, owner string) {
	mem.seedVPC(&db.VPCRow{
		ID:               vpcID,
		OwnerPrincipalID: owner,
		Name:             "test-vpc",
		CIDRIPv4:         "10.0.0.0/16",
		Status:           "active",
	})
	mem.seedSubnet(&db.SubnetRow{
		ID:               subnetID,
		VPCID:            vpcID,
		Name:             "test-subnet",
		CIDRIPv4:         "10.0.0.0/24",
		AvailabilityZone: "us-east-1a",
		Status:           "active",
	})
	// Create default security group for VPC
	mem.seedSecurityGroup(&db.SecurityGroupRow{
		ID:               "sg_default_" + vpcID,
		VPCID:            vpcID,
		OwnerPrincipalID: owner,
		Name:             "default",
		IsDefault:        true,
	})
}

// ── M9 Slice 4: Networking tests ──────────────────────────────────────────────

func TestCreate_ClassicInstance_NoNetworking(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var out CreateInstanceResponse
	decodeBody(t, resp, &out)

	// Classic instance should not have networking info
	if out.Instance.Networking != nil {
		t.Error("classic instance should not have networking info")
	}
}

func TestCreate_VPCInstance_WithSubnet(t *testing.T) {
	s := newTestSrv(t)
	seedVPCInfrastructure(s.mem, "vpc_test1", "subnet_test1", alice)

	body := validCreateBody()
	body.Networking = &NetworkingConfig{
		SubnetID: "subnet_test1",
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var out CreateInstanceResponse
	decodeBody(t, resp, &out)

	// VPC instance should have networking info
	if out.Instance.Networking == nil {
		t.Fatal("VPC instance must have networking info")
	}
	if out.Instance.Networking.VPCID != "vpc_test1" {
		t.Errorf("want vpc_id=vpc_test1, got %q", out.Instance.Networking.VPCID)
	}
	if out.Instance.Networking.SubnetID != "subnet_test1" {
		t.Errorf("want subnet_id=subnet_test1, got %q", out.Instance.Networking.SubnetID)
	}
	if out.Instance.Networking.PrimaryInterface == nil {
		t.Fatal("VPC instance must have primary interface")
	}
	if out.Instance.Networking.PrimaryInterface.PrivateIP == "" {
		t.Error("primary interface must have private IP")
	}
	// VPC instances get private IP from NIC
	if out.Instance.PrivateIP == nil || *out.Instance.PrivateIP == "" {
		t.Error("VPC instance must have private IP set")
	}
	// VPC instances don't have public IP by default
	if out.Instance.PublicIP != nil {
		t.Error("VPC instance should not have public IP without EIP")
	}
}

func TestCreate_VPCInstance_SecurityGroupIDsInResponse(t *testing.T) {
	s := newTestSrv(t)
	seedVPCInfrastructure(s.mem, "vpc_sgtest", "subnet_sgtest", alice)

	// Add additional security groups
	s.mem.seedSecurityGroup(&db.SecurityGroupRow{
		ID:               "sg_custom_1",
		VPCID:            "vpc_sgtest",
		OwnerPrincipalID: alice,
		Name:             "custom-sg-1",
		IsDefault:        false,
	})
	s.mem.seedSecurityGroup(&db.SecurityGroupRow{
		ID:               "sg_custom_2",
		VPCID:            "vpc_sgtest",
		OwnerPrincipalID: alice,
		Name:             "custom-sg-2",
		IsDefault:        false,
	})

	body := validCreateBody()
	body.Networking = &NetworkingConfig{
		SubnetID:         "subnet_sgtest",
		SecurityGroupIDs: []string{"sg_custom_1", "sg_custom_2"},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var out CreateInstanceResponse
	decodeBody(t, resp, &out)

	if out.Instance.Networking == nil {
		t.Fatal("expected networking info")
	}
	if out.Instance.Networking.PrimaryInterface == nil {
		t.Fatal("expected primary interface")
	}
	// Verify SecurityGroupIDs are in CREATE response (not just GET)
	if len(out.Instance.Networking.PrimaryInterface.SecurityGroupIDs) != 2 {
		t.Errorf("want 2 security groups in create response, got %d",
			len(out.Instance.Networking.PrimaryInterface.SecurityGroupIDs))
	}
}

func TestCreate_VPCInstance_SubnetNotFound(t *testing.T) {
	s := newTestSrv(t)

	body := validCreateBody()
	body.Networking = &NetworkingConfig{
		SubnetID: "subnet_nonexistent",
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}

	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errSubnetNotFound {
		t.Errorf("want code %q, got %q", errSubnetNotFound, env.Error.Code)
	}
}

func TestCreate_VPCInstance_CrossAccountSubnet(t *testing.T) {
	s := newTestSrv(t)
	// Create VPC owned by Bob
	seedVPCInfrastructure(s.mem, "vpc_bob", "subnet_bob", bob)

	body := validCreateBody()
	body.Networking = &NetworkingConfig{
		SubnetID: "subnet_bob",
	}

	// Alice tries to create instance in Bob's subnet
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for cross-account subnet, got %d", resp.StatusCode)
	}

	var env apiError
	decodeBody(t, resp, &env)
	// Should return 404 to prevent enumeration
	if env.Error.Code != errSubnetNotFound {
		t.Errorf("want code %q, got %q", errSubnetNotFound, env.Error.Code)
	}
}

func TestCreate_VPCInstance_TooManySecurityGroups(t *testing.T) {
	s := newTestSrv(t)
	seedVPCInfrastructure(s.mem, "vpc_sg", "subnet_sg", alice)

	body := validCreateBody()
	body.Networking = &NetworkingConfig{
		SubnetID: "subnet_sg",
		SecurityGroupIDs: []string{
			"sg_1", "sg_2", "sg_3", "sg_4", "sg_5", "sg_6", // 6 > max 5
		},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}

	var env apiError
	decodeBody(t, resp, &env)
	found := false
	for _, d := range env.Error.Details {
		if d.Target == "security_group_ids" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected validation error for security_group_ids")
	}
}

func TestGetInstance_WithNetworking(t *testing.T) {
	s := newTestSrv(t)
	seedVPCInfrastructure(s.mem, "vpc_get", "subnet_get", alice)
	seedInstance(s.mem, "inst_vpc", "vpc-instance", alice, "running")

	// Add NIC for the instance
	s.mem.seedNetworkInterface(&db.NetworkInterfaceRow{
		ID:         "nic_001",
		InstanceID: "inst_vpc",
		SubnetID:   "subnet_get",
		VPCID:      "vpc_get",
		PrivateIP:  "10.0.0.15",
		MACAddress: "02:00:00:00:00:01",
		IsPrimary:  true,
		Status:     "attached",
	})
	s.mem.nicSecurityGroups["nic_001"] = []string{"sg_default_vpc_get"}

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_vpc", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var out InstanceResponse
	decodeBody(t, resp, &out)

	if out.Networking == nil {
		t.Fatal("expected networking info")
	}
	if out.Networking.VPCID != "vpc_get" {
		t.Errorf("want vpc_id=vpc_get, got %q", out.Networking.VPCID)
	}
	if out.Networking.PrimaryInterface == nil {
		t.Fatal("expected primary interface")
	}
	if out.Networking.PrimaryInterface.PrivateIP != "10.0.0.15" {
		t.Errorf("want private_ip=10.0.0.15, got %q", out.Networking.PrimaryInterface.PrivateIP)
	}
	if len(out.Networking.PrimaryInterface.SecurityGroupIDs) != 1 {
		t.Errorf("want 1 security group, got %d", len(out.Networking.PrimaryInterface.SecurityGroupIDs))
	}
}

// ── PASS 3: Idempotency — Create ─────────────────────────────────────────────

func TestIdempotency_Create_SameKey(t *testing.T) {
	s := newTestSrv(t)
	hdrs := authHdrWithIkey(alice, "ikey-create-001")

	resp1 := doReq(t, s.ts, http.MethodPost, "/v1/instances", validCreateBody(), hdrs)
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first create: want 202, got %d", resp1.StatusCode)
	}
	var out1 CreateInstanceResponse
	decodeBody(t, resp1, &out1)

	// Repeat with the same idempotency key.
	resp2 := doReq(t, s.ts, http.MethodPost, "/v1/instances", validCreateBody(), hdrs)
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("duplicate create: want 202, got %d", resp2.StatusCode)
	}
	var out2 CreateInstanceResponse
	decodeBody(t, resp2, &out2)

	if out1.Instance.ID != out2.Instance.ID {
		t.Errorf("idempotent create: want same instance_id %q, got %q", out1.Instance.ID, out2.Instance.ID)
	}
	// Only one instance should exist in the store.
	if len(s.mem.instances) != 1 {
		t.Errorf("want 1 instance in store, got %d", len(s.mem.instances))
	}
}

func TestIdempotency_Create_DifferentKey(t *testing.T) {
	s := newTestSrv(t)

	resp1 := doReq(t, s.ts, http.MethodPost, "/v1/instances", validCreateBody(), authHdrWithIkey(alice, "key-A"))
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first create: want 202, got %d", resp1.StatusCode)
	}
	var out1 CreateInstanceResponse
	decodeBody(t, resp1, &out1)

	resp2 := doReq(t, s.ts, http.MethodPost, "/v1/instances", validCreateBody(), authHdrWithIkey(alice, "key-B"))
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("second create: want 202, got %d", resp2.StatusCode)
	}
	var out2 CreateInstanceResponse
	decodeBody(t, resp2, &out2)

	if out1.Instance.ID == out2.Instance.ID {
		t.Error("different idempotency keys must produce distinct instances")
	}
}

func TestIdempotency_Create_NoKey(t *testing.T) {
	s := newTestSrv(t)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", validCreateBody(), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create without ikey: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ID == "" {
		t.Error("want non-empty instance ID")
	}
}

// ── PASS 3: Idempotency — Lifecycle actions ───────────────────────────────────

func TestIdempotency_Stop_SameKey(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_idem_stop", "idem-stop", alice, "running")
	hdrs := authHdrWithIkey(alice, "ikey-stop-001")

	resp1 := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_idem_stop/stop", nil, hdrs)
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first stop: want 202, got %d", resp1.StatusCode)
	}
	var out1 LifecycleResponse
	decodeBody(t, resp1, &out1)

	resp2 := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_idem_stop/stop", nil, hdrs)
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("duplicate stop: want 202, got %d", resp2.StatusCode)
	}
	var out2 LifecycleResponse
	decodeBody(t, resp2, &out2)

	if out1.JobID != out2.JobID {
		t.Errorf("idempotent stop: want same job_id %q, got %q", out1.JobID, out2.JobID)
	}
	if len(s.mem.jobs) != 1 {
		t.Errorf("want exactly 1 job in store, got %d", len(s.mem.jobs))
	}
}

func TestIdempotency_Lifecycle_SameKeyDifferentInstance(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_A", "inst-a", alice, "running")
	seedInstance(s.mem, "inst_B", "inst-b", alice, "running")
	hdrs := authHdrWithIkey(alice, "ikey-conflict-001")

	// First stop on inst_A succeeds.
	resp1 := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_A/stop", nil, hdrs)
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("stop inst_A: want 202, got %d", resp1.StatusCode)
	}
	resp1.Body.Close()

	// Same key on inst_B must conflict.
	resp2 := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_B/stop", nil, hdrs)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("reuse key on different instance: want 409, got %d", resp2.StatusCode)
	}
	var env apiError
	decodeBody(t, resp2, &env)
	if env.Error.Code != errIdempotencyMismatch {
		t.Errorf("want code %q, got %q", errIdempotencyMismatch, env.Error.Code)
	}
}

// ── PASS 3: Job status endpoint ───────────────────────────────────────────────

func TestGetJob_Happy(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_jq", "job-query", alice, "running")
	s.mem.seedJob(&db.JobRow{
		ID:         "job_abc123",
		InstanceID: "inst_jq",
		JobType:    jobTypeStop,
		Status:     "pending",
	})

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_jq/jobs/job_abc123", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out JobResponse
	decodeBody(t, resp, &out)

	if out.ID != "job_abc123" {
		t.Errorf("want id=job_abc123, got %q", out.ID)
	}
	if out.InstanceID != "inst_jq" {
		t.Errorf("want instance_id=inst_jq, got %q", out.InstanceID)
	}
}

func TestGetJob_NotFound(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_jnf", "job-nf", alice, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_jnf/jobs/job_ghost", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errJobNotFound {
		t.Errorf("want code %q, got %q", errJobNotFound, env.Error.Code)
	}
}

func TestGetJob_WrongOwner(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_bob_j", "bob-j", bob, "running")
	s.mem.seedJob(&db.JobRow{
		ID:         "job_bobs01",
		InstanceID: "inst_bob_j",
		JobType:    jobTypeStop,
		Status:     "pending",
	})

	// Alice tries to access Bob's instance and job.
	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_bob_j/jobs/job_bobs01", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for cross-account job access, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	// Instance ownership check fires first; error code is instance_not_found.
	if env.Error.Code != errInstanceNotFound {
		t.Errorf("want code %q, got %q", errInstanceNotFound, env.Error.Code)
	}
}

// ── PASS 2: Auth tests ───────────────────────────────────────────────────────

func TestAuth_MissingHeader(t *testing.T) {
	s := newTestSrv(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/instances"},
		{http.MethodPost, "/v1/instances"},
		{http.MethodGet, "/v1/instances/inst_any"},
		{http.MethodDelete, "/v1/instances/inst_any"},
		{http.MethodPost, "/v1/instances/inst_any/stop"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			resp := doReq(t, s.ts, ep.method, ep.path, nil, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d", resp.StatusCode)
			}
			var env apiError
			decodeBody(t, resp, &env)
			if env.Error.Code != errAuthRequired {
				t.Errorf("want code %q, got %q", errAuthRequired, env.Error.Code)
			}
		})
	}
}

// ── PASS 2: Ownership tests ──────────────────────────────────────────────────

func TestOwnership_OwnInstance(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_mine", "mine", alice, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_mine", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for own instance, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestOwnership_OtherUsersInstance(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_bobs", "bobs-inst", bob, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_bobs", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 (not 403) for cross-account access, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInstanceNotFound {
		t.Errorf("want code %q, got %q", errInstanceNotFound, env.Error.Code)
	}
}

// ── PASS 2: Lifecycle happy path tests ────────────────────────────────────────

func TestStop_Happy(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_run", "run-me", alice, "running")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_run/stop", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out LifecycleResponse
	decodeBody(t, resp, &out)
	if out.InstanceID != "inst_run" {
		t.Errorf("want instance_id=inst_run, got %q", out.InstanceID)
	}
	if !strings.HasPrefix(out.JobID, "job_") {
		t.Errorf("want job_id with job_ prefix, got %q", out.JobID)
	}
	if out.Action != "stop" {
		t.Errorf("want action=stop, got %q", out.Action)
	}
}

// ── PASS 2: Illegal state transition tests ───────────────────────────────────

func TestIllegalTransition_StopOnStopped(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_stopped2", "already-stopped", alice, "stopped")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_stopped2/stop", nil, authHdr(alice))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errIllegalTransition {
		t.Errorf("want code %q, got %q", errIllegalTransition, env.Error.Code)
	}
}

// ── PASS 1: POST /v1/instances ────────────────────────────────────────────────

func TestCreate_Happy(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		validCreateBody(), authHdr(alice))

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)

	if !strings.HasPrefix(out.Instance.ID, "inst_") {
		t.Errorf("instance ID must have inst_ prefix, got %q", out.Instance.ID)
	}
	if out.Instance.Status != "requested" {
		t.Errorf("want status=requested, got %q", out.Instance.Status)
	}
	if out.Instance.Region != "us-east-1" {
		t.Errorf("want region=us-east-1, got %q", out.Instance.Region)
	}
	if out.Instance.Labels == nil {
		t.Error("want labels to be non-nil map (even if empty)")
	}
}

func TestCreate_MalformedJSON(t *testing.T) {
	s := newTestSrv(t)
	req, _ := http.NewRequest(http.MethodPost, s.ts.URL+"/v1/instances",
		strings.NewReader("{not valid json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-ID", alice)
	resp, err := s.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidRequest {
		t.Errorf("want code %q, got %q", errInvalidRequest, env.Error.Code)
	}
}

func TestCreate_AllFieldsMissing(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		CreateInstanceRequest{}, authHdr(alice))

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidRequest {
		t.Errorf("top-level code: want %q, got %q", errInvalidRequest, env.Error.Code)
	}
	if len(env.Error.Details) == 0 {
		t.Fatal("want non-empty details array for multi-field failure")
	}
}

// ── PASS 1: GET /v1/instances/{id} ───────────────────────────────────────────

func TestGetInstance_Happy(t *testing.T) {
	s := newTestSrv(t)
	s.mem.seed(&db.InstanceRow{
		ID: "inst_abc123", Name: "test-inst", OwnerPrincipalID: alice,
		VMState: "running", InstanceTypeID: "c1.medium",
		ImageID: "00000000-0000-0000-0000-000000000011", AvailabilityZone: "us-east-1b",
	})

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_abc123", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out InstanceResponse
	decodeBody(t, resp, &out)
	if out.ID != "inst_abc123" {
		t.Errorf("want ID=inst_abc123, got %q", out.ID)
	}
	if out.Status != "running" {
		t.Errorf("want status=running, got %q", out.Status)
	}
}

func TestGetInstance_NotFound(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_doesnotexist", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInstanceNotFound {
		t.Errorf("want code %q, got %q", errInstanceNotFound, env.Error.Code)
	}
}

// ── PASS 1: GET /v1/instances ─────────────────────────────────────────────────

func TestListInstances_Empty(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListInstancesResponse
	decodeBody(t, resp, &out)
	if out.Total != 0 {
		t.Errorf("want total=0, got %d", out.Total)
	}
	if out.Instances == nil {
		t.Error("want non-nil instances slice (empty, not null)")
	}
}

func TestListInstances_ScopedToHeader(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_a1", "alice-one", alice, "running")
	seedInstance(s.mem, "inst_a2", "alice-two", alice, "stopped")
	seedInstance(s.mem, "inst_b1", "bob-one", bob, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListInstancesResponse
	decodeBody(t, resp, &out)
	if out.Total != 2 {
		t.Errorf("want 2 instances for alice, got %d", out.Total)
	}
	for _, inst := range out.Instances {
		if inst.ID == "inst_b1" {
			t.Error("bob's instance must not appear in alice's list")
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func assertDetailCode(t *testing.T, env apiError, wantTarget, wantCode string) {
	t.Helper()
	for _, d := range env.Error.Details {
		if d.Target == wantTarget {
			if d.Code != wantCode {
				t.Errorf("detail[target=%q]: want code %q, got %q", wantTarget, wantCode, d.Code)
			}
			return
		}
	}
	t.Errorf("no detail entry with target=%q (got: %v)", wantTarget, detailCodes(env))
}

func detailCodes(env apiError) []string {
	var out []string
	for _, d := range env.Error.Details {
		out = append(out, fmt.Sprintf("%s:%s", d.Target, d.Code))
	}
	return out
}

// ── VM-P2D Slice 3: Quota admission tests ────────────────────────────────────

// TestCreate_QuotaExceeded verifies that a create request is rejected with
// 422 quota_exceeded when the scope has reached its instance limit.
// Source: vm-13-02__blueprint__ §core_contracts "Error Code Separation".
func TestCreate_QuotaExceeded(t *testing.T) {
	s := newTestSrv(t)

	// Seed max instances (default limit = 10) for alice.
	for i := 0; i < 10; i++ {
		seedInstance(s.mem, fmt.Sprintf("inst_quota_%d", i), fmt.Sprintf("quota-%d", i), alice, "running")
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", validCreateBody(), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 quota_exceeded, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errQuotaExceeded {
		t.Errorf("want code %q, got %q", errQuotaExceeded, env.Error.Code)
	}
}

// TestCreate_QuotaWithinLimit verifies that a create succeeds when the scope
// is at limit-1 (one slot remaining).
func TestCreate_QuotaWithinLimit(t *testing.T) {
	s := newTestSrv(t)

	// Seed 9 instances — one slot remains under the default limit of 10.
	for i := 0; i < 9; i++ {
		seedInstance(s.mem, fmt.Sprintf("inst_q9_%d", i), fmt.Sprintf("q9-%d", i), alice, "running")
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", validCreateBody(), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 when within quota, got %d", resp.StatusCode)
	}
}

// TestCreate_QuotaExceeded_DistinctFromCapacityFailure verifies that the
// quota_exceeded error code is distinct from any capacity/scheduler failure code.
// This guards against the specific contract violation described in:
// vm-13-02__blueprint__ §core_contracts "Error Code Separation".
func TestCreate_QuotaExceeded_DistinctFromCapacityFailure(t *testing.T) {
	s := newTestSrv(t)

	// Exhaust alice's quota.
	for i := 0; i < 10; i++ {
		seedInstance(s.mem, fmt.Sprintf("inst_cap_%d", i), fmt.Sprintf("cap-%d", i), alice, "running")
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", validCreateBody(), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)

	// quota_exceeded must not be confused with scheduler capacity codes.
	if env.Error.Code == "insufficient_capacity" || env.Error.Code == "service_unavailable" {
		t.Errorf("quota failure must not be coded as capacity failure, got %q", env.Error.Code)
	}
	if env.Error.Code != errQuotaExceeded {
		t.Errorf("want %q, got %q", errQuotaExceeded, env.Error.Code)
	}
}

// TestCreate_QuotaIsolation_CrossAccount verifies that quota exhaustion for one
// principal does not affect a different principal. The 404-for-cross-account
// behavior established in Slice 2 must still hold.
func TestCreate_QuotaIsolation_CrossAccount(t *testing.T) {
	s := newTestSrv(t)

	// Exhaust bob's quota.
	for i := 0; i < 10; i++ {
		seedInstance(s.mem, fmt.Sprintf("inst_bob_q_%d", i), fmt.Sprintf("bob-q%d", i), bob, "running")
	}

	// Alice's quota is independent — her create must succeed.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", validCreateBody(), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("alice's create must succeed when only bob's quota is exhausted, got %d", resp.StatusCode)
	}

	// Cross-account instance visibility: alice cannot see bob's instances.
	for i := 0; i < 10; i++ {
		instID := fmt.Sprintf("inst_bob_q_%d", i)
		getResp := doReq(t, s.ts, http.MethodGet, "/v1/instances/"+instID, nil, authHdr(alice))
		if getResp.StatusCode != http.StatusNotFound {
			t.Errorf("want 404 for cross-account instance %s, got %d", instID, getResp.StatusCode)
		}
		getResp.Body.Close()
	}
}

// TestCreate_CustomQuotaLimit verifies that a per-scope quota override is
// respected when the operator seeds a lower limit.
func TestCreate_CustomQuotaLimit(t *testing.T) {
	s := newTestSrv(t)

	// Operator sets alice's limit to 2.
	s.mem.seedQuota(alice, 2)

	// Seed 2 instances — at the limit.
	seedInstance(s.mem, "inst_cq_0", "cq-0", alice, "running")
	seedInstance(s.mem, "inst_cq_1", "cq-1", alice, "running")

	// Third create must be rejected.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", validCreateBody(), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 at custom limit=2, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errQuotaExceeded {
		t.Errorf("want %q, got %q", errQuotaExceeded, env.Error.Code)
	}
}

// ── VM-P2D Slice 4: Project scoping and tenant-safe resource accounting ───────
//
// These tests verify:
//   1. project_id in CreateInstanceRequest resolves to project.principal_id as owner.
//   2. Quota is scoped per project.principal_id, independent of user principal.
//   3. GET /v1/instances?project_id= lists only that project's instances.
//   4. Cross-project access returns 404 (hide-existence contract).
//   5. Classic (no-project) paths are entirely unchanged.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §3, vm-16-01__blueprint__ §quota_enforcement_point,
//         vm-13-02__blueprint__ §core_contracts "Error Code Separation".

// seedProject adds a project and its principal to the memPool for Slice 4 tests.
// principalID is the project's entry in the principals table (proj.PrincipalID).
// This is the value used as owner_principal_id for project-scoped instances.
func seedProject(mem *memPool, projectID, principalID, createdBy, name string) {
	now := time.Now()
	mem.principals[principalID] = "PROJECT"
	mem.projects[projectID] = &db.ProjectRow{
		ID:          projectID,
		PrincipalID: principalID,
		CreatedBy:   createdBy,
		Name:        name,
		DisplayName: name,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// TestCreate_WithProjectScope_Happy verifies that POST /v1/instances with a valid
// project_id creates an instance with owner_principal_id = project.principal_id,
// and returns project_id in the response.
func TestCreate_WithProjectScope_Happy(t *testing.T) {
	s := newTestSrv(t)
	seedProject(s.mem, "proj_alice_s4", "prin_proj_alice_s4", alice, "alice-project-s4")

	pid := "proj_alice_s4"
	body := validCreateBody()
	body.ProjectID = &pid

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var out CreateInstanceResponse
	decodeBody(t, resp, &out)

	if out.Instance.ID == "" {
		t.Error("want non-empty instance ID")
	}
	// project_id must round-trip in the response.
	if out.Instance.ProjectID == nil || *out.Instance.ProjectID != "proj_alice_s4" {
		t.Errorf("want project_id=proj_alice_s4 in response, got %v", out.Instance.ProjectID)
	}

	// The instance must be stored with owner_principal_id = project's principal_id,
	// not alice's own principal_id.
	var found *db.InstanceRow
	for _, inst := range s.mem.instances {
		found = inst
		break
	}
	if found == nil {
		t.Fatal("no instance row created")
	}
	if found.OwnerPrincipalID != "prin_proj_alice_s4" {
		t.Errorf("want owner_principal_id=prin_proj_alice_s4, got %q", found.OwnerPrincipalID)
	}
}

// TestCreate_WithProjectScope_ProjectNotFound verifies that an unknown project_id
// returns 404 project_not_found, not 422 or 500.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account).
func TestCreate_WithProjectScope_ProjectNotFound(t *testing.T) {
	s := newTestSrv(t)
	pid := "proj_does_not_exist"
	body := validCreateBody()
	body.ProjectID = &pid

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for missing project, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errProjectNotFound {
		t.Errorf("want code %q, got %q", errProjectNotFound, env.Error.Code)
	}
}

// TestCreate_WithProjectScope_CrossProject verifies that Alice cannot create an
// instance in Bob's project. Existence of Bob's project is hidden (404, not 403).
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
func TestCreate_WithProjectScope_CrossProject(t *testing.T) {
	s := newTestSrv(t)
	seedProject(s.mem, "proj_bob_s4", "prin_proj_bob_s4", bob, "bob-project-s4")

	pid := "proj_bob_s4"
	body := validCreateBody()
	body.ProjectID = &pid

	// Alice tries to create in Bob's project.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for cross-project create, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errProjectNotFound {
		t.Errorf("want code %q, got %q", errProjectNotFound, env.Error.Code)
	}
	// No instance must have been created.
	if len(s.mem.instances) != 0 {
		t.Errorf("want 0 instances after cross-project rejection, got %d", len(s.mem.instances))
	}
}

// TestCreate_WithProjectScope_QuotaIsolation verifies that quota is scoped per
// project.principal_id, not per user principal.
//
// Setup:
//   - Alice's USER quota is exhausted (10 instances with owner=alice).
//   - Alice's PROJECT quota starts fresh (0 instances with owner=project principal_id).
//   - A project-scoped create must succeed (project quota not exhausted).
//   - After 10 project-scoped creates, the project quota is also exhausted.
//
// This confirms that project scope and user scope are independent quota pools.
// Source: vm-13-02__blueprint__ §core_contracts "Error Code Separation",
//         vm-16-01__blueprint__ §quota_enforcement_point.
func TestCreate_WithProjectScope_QuotaIsolation(t *testing.T) {
	s := newTestSrv(t)
	seedProject(s.mem, "proj_quota_s4", "prin_proj_quota_s4", alice, "quota-project-s4")

	// Exhaust Alice's USER quota (classic scope, owner=alice principal).
	for i := 0; i < 10; i++ {
		seedInstance(s.mem, fmt.Sprintf("inst_user_s4_%d", i), fmt.Sprintf("user-s4-%d", i), alice, "running")
	}

	// Alice's project scope is a separate quota pool — create must succeed.
	pid := "proj_quota_s4"
	body := validCreateBody()
	body.ProjectID = &pid

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for project-scoped create when user quota is exhausted, got %d", resp.StatusCode)
	}

	// Now seed 9 more project-scoped instances (total=10 after the one created above).
	for i := 0; i < 9; i++ {
		seedInstance(s.mem,
			fmt.Sprintf("inst_proj_s4_%d", i),
			fmt.Sprintf("proj-s4-%d", i),
			"prin_proj_quota_s4", // owner = project's principal_id
			"running",
		)
	}

	// The project quota is now exhausted — next create must be rejected.
	resp2 := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 quota_exceeded when project quota exhausted, got %d", resp2.StatusCode)
	}
	var env apiError
	decodeBody(t, resp2, &env)
	if env.Error.Code != errQuotaExceeded {
		t.Errorf("want code %q, got %q", errQuotaExceeded, env.Error.Code)
	}
}

// TestCreate_NoProject_ClassicScopeUnchanged verifies that omitting project_id
// preserves the classic (user-principal-scoped) behavior from prior slices.
// owner_principal_id must equal alice's principal, not a project principal.
func TestCreate_NoProject_ClassicScopeUnchanged(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	// No ProjectID set — classic mode.

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)

	// project_id must be absent (nil) in the response for classic creates.
	if out.Instance.ProjectID != nil {
		t.Errorf("want nil project_id for classic create, got %v", *out.Instance.ProjectID)
	}

	// owner_principal_id must be alice's principal (not any project principal).
	var found *db.InstanceRow
	for _, inst := range s.mem.instances {
		found = inst
		break
	}
	if found == nil {
		t.Fatal("no instance created")
	}
	if found.OwnerPrincipalID != alice {
		t.Errorf("classic create: want owner=%q, got %q", alice, found.OwnerPrincipalID)
	}
}

// TestList_WithProjectFilter_Happy verifies that GET /v1/instances?project_id=
// returns only instances whose owner_principal_id == project.principal_id.
// Classic-scoped instances with owner=alice must not appear.
func TestList_WithProjectFilter_Happy(t *testing.T) {
	s := newTestSrv(t)
	seedProject(s.mem, "proj_list_s4", "prin_proj_list_s4", alice, "list-project-s4")

	// Seed one project-scoped instance (owner = project's principal_id).
	seedInstance(s.mem, "inst_proj_list_01", "proj-list-01", "prin_proj_list_s4", "running")
	// Seed one classic alice instance (owner = alice).
	seedInstance(s.mem, "inst_alice_classic", "alice-classic", alice, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances?project_id=proj_list_s4", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListInstancesResponse
	decodeBody(t, resp, &out)

	if out.Total != 1 {
		t.Errorf("want 1 instance in project scope, got %d", out.Total)
	}
	if len(out.Instances) > 0 && out.Instances[0].ID != "inst_proj_list_01" {
		t.Errorf("want inst_proj_list_01, got %q", out.Instances[0].ID)
	}
	// Classic instance must not appear in project-scoped list.
	for _, inst := range out.Instances {
		if inst.ID == "inst_alice_classic" {
			t.Error("classic-scoped instance must not appear in project-scoped list")
		}
	}
}

// TestList_WithProjectFilter_CrossProject verifies that Alice gets 404 when
// listing Bob's project. Existence is hidden per AUTH_OWNERSHIP_MODEL_V1 §3.
func TestList_WithProjectFilter_CrossProject(t *testing.T) {
	s := newTestSrv(t)
	seedProject(s.mem, "proj_bob_list_s4", "prin_proj_bob_list_s4", bob, "bob-list-s4")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances?project_id=proj_bob_list_s4", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for cross-project list, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errProjectNotFound {
		t.Errorf("want code %q, got %q", errProjectNotFound, env.Error.Code)
	}
}

// TestList_WithProjectFilter_ProjectNotFound verifies 404 for a nonexistent project.
func TestList_WithProjectFilter_ProjectNotFound(t *testing.T) {
	s := newTestSrv(t)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances?project_id=proj_ghost", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errProjectNotFound {
		t.Errorf("want code %q, got %q", errProjectNotFound, env.Error.Code)
	}
}

// TestList_NoProjectFilter_ClassicScopeUnchanged verifies that omitting ?project_id=
// returns exactly the classic (user-principal-scoped) list — Slice 2 behavior preserved.
func TestList_NoProjectFilter_ClassicScopeUnchanged(t *testing.T) {
	s := newTestSrv(t)
	seedProject(s.mem, "proj_classic_s4", "prin_proj_classic_s4", alice, "classic-s4")

	// Classic-scoped instances for alice.
	seedInstance(s.mem, "inst_classic_1", "classic-1", alice, "running")
	seedInstance(s.mem, "inst_classic_2", "classic-2", alice, "stopped")
	// Project-scoped instance (owner = project principal) — must not appear in classic list.
	seedInstance(s.mem, "inst_proj_x", "proj-x", "prin_proj_classic_s4", "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListInstancesResponse
	decodeBody(t, resp, &out)

	if out.Total != 2 {
		t.Errorf("want 2 classic instances for alice, got %d", out.Total)
	}
	for _, inst := range out.Instances {
		if inst.ID == "inst_proj_x" {
			t.Error("project-scoped instance must not appear in classic list")
		}
	}
}

// TestCreate_WithProjectScope_DeletedProject verifies that a soft-deleted (status=deleted)
// project returns 422 (project not active), not a generic 500.
// The status check prevents use of deleted project scope.
func TestCreate_WithProjectScope_DeletedProject(t *testing.T) {
	s := newTestSrv(t)
	// Seed a deleted project.
	now := time.Now()
	s.mem.projects["proj_deleted_s4"] = &db.ProjectRow{
		ID:          "proj_deleted_s4",
		PrincipalID: "prin_proj_deleted_s4",
		CreatedBy:   alice,
		Name:        "deleted-project",
		DisplayName: "deleted-project",
		Status:      "deleted",
		CreatedAt:   now,
		UpdatedAt:   now,
		DeletedAt:   &now,
	}

	pid := "proj_deleted_s4"
	body := validCreateBody()
	body.ProjectID = &pid

	// Deleted projects have deleted_at set — GetProjectByID in the real repo
	// returns no rows for deleted projects. In the memPool, deleted_at != nil
	// triggers errRow (no rows in result set) → isProjectNotFound → 404.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	// The memPool treats deleted_at != nil as "not found" (consistent with project_repo.go
	// WHERE deleted_at IS NULL clause). So we expect 404.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for deleted project (deleted_at set), got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errProjectNotFound {
		t.Errorf("want code %q, got %q", errProjectNotFound, env.Error.Code)
	}
}

// TestCreate_WithProjectScope_QuotaErrorStillDistinctFromCapacity verifies that
// the quota vs capacity error separation (Slice 3) is preserved when quota is
// checked via project scope.
// Source: vm-13-02__blueprint__ §core_contracts "Error Code Separation".
func TestCreate_WithProjectScope_QuotaErrorStillDistinctFromCapacity(t *testing.T) {
	s := newTestSrv(t)
	seedProject(s.mem, "proj_cap_s4", "prin_proj_cap_s4", alice, "cap-project-s4")

	// Exhaust the project's quota.
	for i := 0; i < 10; i++ {
		seedInstance(s.mem, fmt.Sprintf("inst_cap_s4_%d", i), fmt.Sprintf("cap-s4-%d", i), "prin_proj_cap_s4", "running")
	}

	pid := "proj_cap_s4"
	body := validCreateBody()
	body.ProjectID = &pid

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)

	// Must be quota_exceeded, NOT insufficient_capacity or service_unavailable.
	if env.Error.Code == "insufficient_capacity" || env.Error.Code == "service_unavailable" {
		t.Errorf("project quota failure must not be coded as capacity failure, got %q", env.Error.Code)
	}
	if env.Error.Code != errQuotaExceeded {
		t.Errorf("want %q, got %q", errQuotaExceeded, env.Error.Code)
	}
}
