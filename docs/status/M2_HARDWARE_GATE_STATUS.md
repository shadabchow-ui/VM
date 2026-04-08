# M2 Hardware Gate Status

## Automated/code gate
M2 automated checks passed locally.

## Current machine
Current validation machine is macOS and is being treated as a dev/control box only.

## Hardware gate status
Real M2 hardware gate (H1-H4) is deferred until execution on a Linux hypervisor host with:
- KVM
- Firecracker
- qemu-img
- ip / iptables
- TAP device permissions
- reachable base image path

## Latest observed local failure
Instance provisioning progressed through host selection and host-agent call, but failed at readiness:

- step6 readiness: SSH port on 10.0.0.1 did not open within 2m0s

Rollback then cleaned up instance resources.

## Interpretation
This does not block code progress on macOS.
It blocks only real-hypervisor validation of H1-H4.

## Next milestone path on macOS
Proceed with M3 code implementation only.
Do not claim M2 hardware gate complete until Linux validation is run.
