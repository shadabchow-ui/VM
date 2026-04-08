# M3 Status

## Scope completed on macOS dev box
- INSTANCE_STOP implemented
- INSTANCE_START implemented
- INSTANCE_REBOOT implemented
- lifecycle/handler tests added
- go test ./... passes
- go build ./... passes

## Current limitation
Real Linux/KVM hardware validation is still deferred.
This repo state represents M3 code/test completion on macOS, not final real-hypervisor validation.

## Next likely milestone path
- Keep M2 hardware validation queued for Linux
- Decide whether to begin M4 reconciler work or prepare Linux bring-up for real lifecycle execution
