# Compute Platform — Curl Golden Path

This document provides exact curl commands to exercise the Compute Platform API.
All examples use `http://localhost:9090` (default resource-manager listen address).

**Prerequisites:**
- Resource Manager running (`make run-resource-manager` or equivalent)
- The `X-Principal-ID` header set to your account's principal UUID

## Setup

```bash
BASE="http://localhost:9090"
PRINCIPAL="00000000-0000-0000-0000-000000000001"
AUTH="-H 'X-Principal-ID: $PRINCIPAL'"
IDEM_KEY="$(uuidgen | tr '[:upper:]' '[:lower:]')"
```

> **Note:** Phase 1 auth uses `X-Principal-ID` header. Phase 2 targets SigV4 signing.
> The internal CLI (`tools/internal-cli/`) provides higher-level wrappers for
> create/delete operations with polling.

## API discovery

```bash
# Version info
curl -s $BASE/v1/version | jq .

# OpenAPI spec
curl -s $BASE/v1/openapi.json | jq .

# Health probe
curl -s $BASE/healthz | jq .
```

## Instances

### List instances

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances | jq .
```

### List instances scoped to a project

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" \
  "$BASE/v1/instances?project_id=proj_XXXX" | jq .
```

### Create an instance

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $IDEM_KEY" \
  $BASE/v1/instances \
  -d '{
    "name": "my-first-vm",
    "instance_type": "gp1.small",
    "image_id": "00000000-0000-0000-0000-000000000010",
    "availability_zone": "us-east-1a",
    "ssh_key_name": "my-key"
  }' | jq .
```

Response: `202 Accepted` with `instance.id` and `warnings` (if any, e.g. deprecated image).

### Create an instance using image family alias

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/instances \
  -d '{
    "name": "my-ubuntu-vm",
    "instance_type": "gp1.small",
    "image_family": {"family_name": "ubuntu"},
    "availability_zone": "us-east-1a",
    "ssh_key_name": "my-key"
  }' | jq .
```

### Create an instance with networking (VPC subnet + security groups)

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/instances \
  -d '{
    "name": "my-networked-vm",
    "instance_type": "gp1.small",
    "image_id": "00000000-0000-0000-0000-000000000010",
    "availability_zone": "us-east-1a",
    "ssh_key_name": "my-key",
    "networking": {
      "subnet_id": "subnet_XXXX",
      "security_group_ids": ["sg_XXXX"]
    }
  }' | jq .
```

### Describe an instance

```bash
INSTANCE_ID="inst_XXXX"
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$INSTANCE_ID | jq .
```

### Stop an instance

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/instances/$INSTANCE_ID/stop | jq .
```

### Start an instance

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/instances/$INSTANCE_ID/start | jq .
```

### Reboot an instance

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/instances/$INSTANCE_ID/reboot | jq .
```

### Delete an instance

```bash
curl -s -X DELETE \
  -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/instances/$INSTANCE_ID | jq .
```

### Poll job status

```bash
JOB_ID="job_XXXX"
curl -s -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/instances/$INSTANCE_ID/jobs/$JOB_ID | jq .
```

Job states: `pending` → `in_progress` → `completed` (or `failed`).

### View instance events

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/instances/$INSTANCE_ID/events | jq .
```

Returns up to 100 events, newest first.

## Images

### List images

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/images | jq .
```

### Describe an image

```bash
IMAGE_ID="img_XXXX"
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/images/$IMAGE_ID | jq .
```

### Create image from snapshot

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/images \
  -d '{
    "source_type": "SNAPSHOT",
    "snapshot_id": "snap_XXXX",
    "name": "my-custom-image",
    "os_family": "ubuntu",
    "os_version": "22.04"
  }' | jq .
```

### Deprecate an image

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/images/$IMAGE_ID/deprecate | jq .
```

### Obsolete an image

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/images/$IMAGE_ID/obsolete | jq .
```

### Share an image (grant access to another principal)

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/images/$IMAGE_ID/grants \
  -d '{"grantee_principal_id": "00000000-0000-0000-0000-000000000002"}' | jq .
```

### List image grants

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/images/$IMAGE_ID/grants | jq .
```

### Revoke image access

```bash
GRANTEE="00000000-0000-0000-0000-000000000002"
curl -s -X DELETE \
  -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/images/$IMAGE_ID/grants/$GRANTEE | jq .
```

## Volumes

### List volumes

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/volumes | jq .
```

### Create volume

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/volumes \
  -d '{
    "size_gb": 50,
    "availability_zone": "us-east-1a"
  }' | jq .
```

### Describe volume

```bash
VOL_ID="vol_XXXX"
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/volumes/$VOL_ID | jq .
```

### Delete volume

```bash
curl -s -X DELETE \
  -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/volumes/$VOL_ID | jq .
```

### Attach volume to instance

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/instances/$INSTANCE_ID/volumes \
  -d "{\"volume_id\": \"$VOL_ID\"}" | jq .
```

### List instance volumes

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/instances/$INSTANCE_ID/volumes | jq .
```

### Detach volume

```bash
curl -s -X DELETE \
  -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/instances/$INSTANCE_ID/volumes/$VOL_ID | jq .
```

## Snapshots

### List snapshots

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/snapshots | jq .
```

### Create snapshot

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/snapshots \
  -d '{
    "volume_id": "vol_XXXX",
    "name": "my-snapshot"
  }' | jq .
```

### Describe snapshot

```bash
SNAP_ID="snap_XXXX"
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/snapshots/$SNAP_ID | jq .
```

## SSH Keys

### List SSH keys

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/ssh-keys | jq .
```

### Register SSH key

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/ssh-keys \
  -d '{
    "name": "my-key",
    "public_key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI..."
  }' | jq .
```

> **Note:** RSA keys (`ssh-rsa`) are rejected. Acceptable: `ssh-ed25519`, `ecdsa-sha2-nistp256`, `ecdsa-sha2-nistp384`, `ecdsa-sha2-nistp521`.

### Describe SSH key

```bash
KEY_ID="sshkey_XXXX"
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/ssh-keys/$KEY_ID | jq .
```

### Delete SSH key

```bash
curl -s -X DELETE \
  -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/ssh-keys/$KEY_ID | jq .
```

## Projects

### List projects

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/projects | jq .
```

### Create project

```bash
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/projects \
  -d '{"name": "my-project"}' | jq .
```

### Describe project

```bash
PROJ_ID="proj_XXXX"
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/projects/$PROJ_ID | jq .
```

## VPC Networking

> **Limitation:** VPC/network handler code is implemented and unit-tested but the
> routes ARE NOT yet wired into the production HTTP mux. The endpoints below are
> documented for reference. They will become available once the network route
> registration is integrated into `api.go:routes()`.

```bash
# Create VPC
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/vpcs \
  -d '{"name": "my-vpc", "cidr_ipv4": "10.0.0.0/16"}' | jq .

# List VPCs
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/vpcs | jq .

# Create subnet
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/vpcs/$VPC_ID/subnets \
  -d '{"name": "my-subnet", "cidr_ipv4": "10.0.1.0/24"}' | jq .
```

## Idempotency

All mutating endpoints accept the optional `Idempotency-Key` header. When
provided, duplicate requests return the original response instead of creating
duplicate resources.

```bash
KEY="$(uuidgen | tr '[:upper:]' '[:lower:]')"

# First request — creates
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $KEY" \
  $BASE/v1/instances -d '{"name":"idem-test","instance_type":"gp1.small","image_id":"00000000-0000-0000-0000-000000000010","availability_zone":"us-east-1a","ssh_key_name":"my-key"}' | jq .

# Second request with same key — returns same instance
curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $KEY" \
  $BASE/v1/instances -d '{"name":"idem-test","instance_type":"gp1.small","image_id":"00000000-0000-0000-0000-000000000010","availability_zone":"us-east-1a","ssh_key_name":"my-key"}' | jq .
```

The `Idempotency-Key` is per-job-type. Keys are valid for 24 hours (see
`JOB_MODEL_V1 §6`).

## Internal CLI

The repo includes a minimal internal CLI at `tools/internal-cli/`:

```bash
# Issue bootstrap token for a host
go run ./tools/internal-cli/ issue-bootstrap-token --host-id=host-X

# Create instance and poll until RUNNING
go run ./tools/internal-cli/ create-instance \
  --name=my-vm --image-id=<uuid> --instance-type=c1.small \
  --az=us-east-1a --principal-id=<uuid> --ssh-key="ssh-ed25519 AAAA..." --timeout=300

# Delete instance and poll until DELETED
go run ./tools/internal-cli/ delete-instance --instance-id=inst_XXXX --timeout=120
```

> **Limitation:** The internal CLI connects directly to the database. It does
> not use the HTTP API. A proper HTTP-based CLI client is planned for M7.
