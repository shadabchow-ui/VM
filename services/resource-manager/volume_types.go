package main

// volume_types.go — Request/response types and field-level validation functions
// for the volume HTTP handlers.
//
// Separated from volume_handlers.go to keep the handler file focused on
// business logic and routing.
//
// Volume-specific error code constants are declared in volume_errors.go.
// General codes (errInvalidRequest, errIllegalTransition) are declared in the
// shared handler helpers. Neither set is redeclared here.
//
// The fieldErr struct type and writeAPIErrors function are declared in
// instance_errors.go and available to all files in package main.
// Validation functions return []fieldErr using fieldErr{} struct literals;
// there is no separate fieldError type or two-argument fieldErr() helper.
//
// Source: P2_VOLUME_MODEL.md §8 (API endpoint summary),
//         API_ERROR_CONTRACT_V1 §1 (error envelope),
//         AUTH_OWNERSHIP_MODEL_V1 §3.

import "time"

// ── Request types ─────────────────────────────────────────────────────────────

// CreateVolumeRequest is the request body for POST /v1/volumes.
// Source: P2_VOLUME_MODEL.md §3.2, §8.
type CreateVolumeRequest struct {
	// Name is the human-readable display name for the volume.
	// Required. 1–63 characters.
	Name string `json:"name"`

	// SizeGB is the volume capacity in gibibytes.
	// Required. Must be between 1 and 16384.
	SizeGB int `json:"size_gb"`

	// AvailabilityZone is the AZ in which the volume will be created.
	// Required. Must match a valid AZ for the account's region.
	AvailabilityZone string `json:"availability_zone"`
}

// AttachVolumeRequest is the request body for POST /v1/instances/{id}/volumes.
// Source: P2_VOLUME_MODEL.md §4.2, §8.
type AttachVolumeRequest struct {
	// VolumeID is the ID of the volume to attach.
	// Required.
	VolumeID string `json:"volume_id"`

	// DevicePath is the block device path inside the instance (e.g. "/dev/vdb").
	// Optional — if omitted the system assigns the next available device.
	DevicePath *string `json:"device_path,omitempty"`

	// DeleteOnTermination controls whether the volume is deleted when the
	// instance is terminated. Defaults to false.
	DeleteOnTermination *bool `json:"delete_on_termination,omitempty"`
}

// ── Response types ────────────────────────────────────────────────────────────

// VolumeResponse is the canonical API representation of a volume resource.
// Source: P2_VOLUME_MODEL.md §2.3, §8.
type VolumeResponse struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	Region           string                    `json:"region"`
	AvailabilityZone string                    `json:"availability_zone"`
	SizeGB           int                       `json:"size_gb"`
	Status           string                    `json:"status"`
	Origin           string                    `json:"origin"` // "blank" | "snapshot" | "image"
	SourceDiskID     *string                   `json:"source_disk_id,omitempty"`
	SourceSnapshotID *string                   `json:"source_snapshot_id,omitempty"`
	Attachment       *VolumeAttachmentResponse `json:"attachment,omitempty"`
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
}

// VolumeAttachmentResponse describes the active attachment of a volume.
// Source: P2_VOLUME_MODEL.md §2.4.
type VolumeAttachmentResponse struct {
	InstanceID          string    `json:"instance_id"`
	DevicePath          string    `json:"device_path"`
	DeleteOnTermination bool      `json:"delete_on_termination"`
	AttachedAt          time.Time `json:"attached_at"`
}

// CreateVolumeResponse is the 202 body for POST /v1/volumes.
// Source: P2_VOLUME_MODEL.md §3.2, §8.
type CreateVolumeResponse struct {
	Volume VolumeResponse `json:"volume"`
	JobID  string         `json:"job_id"`
}

// ListVolumesResponse is the 200 body for GET /v1/volumes.
// Source: P2_VOLUME_MODEL.md §8.
type ListVolumesResponse struct {
	Volumes []VolumeResponse `json:"volumes"`
	Total   int              `json:"total"`
}

// VolumeLifecycleResponse is the 202 body for volume lifecycle operations
// (delete, attach, detach). Contains the job ID for polling.
// Source: P2_VOLUME_MODEL.md §4.2, §4.4, §5.2, §8.
type VolumeLifecycleResponse struct {
	VolumeID string `json:"volume_id"`
	JobID    string `json:"job_id"`
	Action   string `json:"action"` // "delete" | "attach" | "detach"
}

// ListInstanceVolumesResponse is the 200 body for GET /v1/instances/{id}/volumes.
// Source: P2_VOLUME_MODEL.md §8.
type ListInstanceVolumesResponse struct {
	Volumes []InstanceVolumeEntry `json:"volumes"`
	Total   int                   `json:"total"`
}

// InstanceVolumeEntry is one element in a ListInstanceVolumesResponse.
// Combines volume metadata with attachment details.
// Source: P2_VOLUME_MODEL.md §8.
type InstanceVolumeEntry struct {
	VolumeID            string    `json:"volume_id"`
	Name                string    `json:"name"`
	SizeGB              int       `json:"size_gb"`
	Status              string    `json:"status"`
	DevicePath          string    `json:"device_path"`
	DeleteOnTermination bool      `json:"delete_on_termination"`
	AttachedAt          time.Time `json:"attached_at"`
}

// ── Field validation ──────────────────────────────────────────────────────────
//
// validateCreateVolumeRequest and validateAttachVolumeRequest return []fieldErr
// (type declared in instance_errors.go). An empty slice means the request is valid.
// Callers pass the slice to writeAPIErrors.
//
// fieldErr is a struct{code, message, target string}. Validation functions
// build instances with struct literals, matching the pattern in
// instance_validation.go and instance_validation_networking.go.
//
// Source: API_ERROR_CONTRACT_V1 §4 (field-level errors).

// validateCreateVolumeRequest validates a CreateVolumeRequest and returns all
// field-level errors found.
func validateCreateVolumeRequest(req *CreateVolumeRequest) []fieldErr {
	var errs []fieldErr

	if req.Name == "" {
		errs = append(errs, fieldErr{
			target:  "name",
			code:    errInvalidRequest,
			message: "Field 'name' is required.",
		})
	} else if len(req.Name) > 63 {
		errs = append(errs, fieldErr{
			target:  "name",
			code:    errInvalidRequest,
			message: "Field 'name' must be 63 characters or fewer.",
		})
	}

	if req.SizeGB <= 0 {
		errs = append(errs, fieldErr{
			target:  "size_gb",
			code:    errInvalidVolumeSize,
			message: "Field 'size_gb' must be a positive integer.",
		})
	} else if req.SizeGB > 16384 {
		errs = append(errs, fieldErr{
			target:  "size_gb",
			code:    errInvalidVolumeSize,
			message: "Field 'size_gb' must be 16384 or less.",
		})
	}

	if req.AvailabilityZone == "" {
		errs = append(errs, fieldErr{
			target:  "availability_zone",
			code:    errInvalidRequest,
			message: "Field 'availability_zone' is required.",
		})
	}

	return errs
}

// validateAttachVolumeRequest validates an AttachVolumeRequest and returns all
// field-level errors found.
func validateAttachVolumeRequest(req *AttachVolumeRequest) []fieldErr {
	var errs []fieldErr

	if req.VolumeID == "" {
		errs = append(errs, fieldErr{
			target:  "volume_id",
			code:    errInvalidRequest,
			message: "Field 'volume_id' is required.",
		})
	}

	return errs
}
