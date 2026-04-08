package domainmodel

import "time"

// InstanceState is the canonical 9-state enum. Source: INSTANCE_MODEL_V1 §3, LIFECYCLE_STATE_MACHINE_V1.
type InstanceState string

const (
	StateRequested   InstanceState = "requested"
	StateProvisioning InstanceState = "provisioning"
	StateRunning     InstanceState = "running"
	StateStopping    InstanceState = "stopping"
	StateStopped     InstanceState = "stopped"
	StateStarting    InstanceState = "starting"
	StateRebooting   InstanceState = "rebooting"
	StateDeleting    InstanceState = "deleting"
	StateDeleted     InstanceState = "deleted"
	StateFailed      InstanceState = "failed"
)

// Instance is the canonical domain object. Source: INSTANCE_MODEL_V1 §2, §4.
type Instance struct {
	ID                string        `db:"id"`
	Name              string        `db:"name"`
	OwnerPrincipalID  string        `db:"owner_principal_id"`
	State             InstanceState `db:"vm_state"`
	InstanceTypeID    string        `db:"instance_type_id"`
	ImageID           string        `db:"image_id"`
	HostID            *string       `db:"host_id"`
	AvailabilityZone  string        `db:"availability_zone"`
	Version           int           `db:"version"`
	CreatedAt         time.Time     `db:"created_at"`
	UpdatedAt         time.Time     `db:"updated_at"`
	DeletedAt         *time.Time    `db:"deleted_at"`
}
