package domainmodel

import "time"

// image.go — Image domain model.
//
// Source: INSTANCE_MODEL_V1.md §7 (Phase 1 image schema),
//         P2_IMAGE_SNAPSHOT_MODEL.md §3 (custom image lifecycle),
//         vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md (state machine),
//         vm-13-01__skill__trusted-image-factory-validation-pipeline.md.

// ImageStatus is the canonical image lifecycle state enum.
// Source: vm-13-01__blueprint__ §Image Catalog and Lifecycle Manager (state machine),
//
//	P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
//
// Admission rules (from vm-13-01__blueprint__ §core_contracts):
//   - ACTIVE and DEPRECATED: launchable in CreateInstance.
//   - OBSOLETE and FAILED: blocked from launch; admission rejects with 422.
//   - PENDING_VALIDATION: blocked; image not yet validated.
type ImageStatus string

const (
	ImageStatusActive            ImageStatus = "ACTIVE"
	ImageStatusDeprecated        ImageStatus = "DEPRECATED"
	ImageStatusObsolete          ImageStatus = "OBSOLETE"
	ImageStatusFailed            ImageStatus = "FAILED"
	ImageStatusPendingValidation ImageStatus = "PENDING_VALIDATION"
)

// IsLaunchable reports whether an image in this status can be used to launch
// a new VM instance.
// Source: vm-13-01__blueprint__ §core_contracts "Image Lifecycle State Enforcement":
//
//	"The VM creation API's admission controller MUST reject any request to create
//	a VM from an image whose state is OBSOLETE or FAILED."
//
// P2_IMAGE_SNAPSHOT_MODEL.md §3.8: "status = ACTIVE" required; DEPRECATED is still launchable.
func (s ImageStatus) IsLaunchable() bool {
	return s == ImageStatusActive || s == ImageStatusDeprecated
}

// ImageVisibility is the access scope of an image.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.7.
type ImageVisibility string

const (
	ImageVisibilityPublic  ImageVisibility = "PUBLIC"
	ImageVisibilityPrivate ImageVisibility = "PRIVATE"
)

// ImageSourceType distinguishes platform-provided images from user-created ones.
// Source: INSTANCE_MODEL_V1.md §7 (source_type field).
type ImageSourceType string

const (
	ImageSourceTypePlatform ImageSourceType = "PLATFORM"
	ImageSourceTypeUser     ImageSourceType = "USER"
	ImageSourceTypeSnapshot ImageSourceType = "SNAPSHOT"
)

// Image is the canonical image domain object.
// Source: INSTANCE_MODEL_V1.md §7, P2_IMAGE_SNAPSHOT_MODEL.md §3.
type Image struct {
	ID               string          `db:"id"`
	Name             string          `db:"name"`
	OSFamily         string          `db:"os_family"`
	OSVersion        string          `db:"os_version"`
	Architecture     string          `db:"architecture"`
	OwnerID          string          `db:"owner_id"`
	Visibility       ImageVisibility `db:"visibility"`
	SourceType       ImageSourceType `db:"source_type"`
	StorageURL       string          `db:"storage_url"`
	MinDiskGB        int             `db:"min_disk_gb"`
	Status           ImageStatus     `db:"status"`
	ValidationStatus string          `db:"validation_status"`
	// Lifecycle timestamps (nullable — set on transition).
	DeprecatedAt *time.Time `db:"deprecated_at"`
	ObsoletedAt  *time.Time `db:"obsoleted_at"`
	// Phase 2: custom image backing snapshot.
	SourceSnapshotID *string   `db:"source_snapshot_id"`
	CreatedAt        time.Time `db:"created_at"`
	UpdatedAt        time.Time `db:"updated_at"`
}
