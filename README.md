# VM Compute Platform

VM is an experimental cloud compute platform project for managing virtual machines through an API-driven control plane.

The project explores how a cloud VM service can organize instance lifecycle, networking, storage, images, host coordination, background repair, and operational visibility.

## What this project is

This repository is an early infrastructure platform project. It is not a finished cloud provider, but it includes meaningful foundations for a managed VM service.

The project is focused on:

- virtual machine lifecycle management
- asynchronous background operations
- scheduling and host coordination
- networking foundations
- detached volume workflows
- image management foundations
- repair and reconciliation loops
- structured API behavior
- future console and developer workflows

## Why it exists

A reliable compute platform needs more than basic create/delete endpoints. It needs clear lifecycle states, durable jobs, ownership rules, background repair, and a separation between the API layer and the host execution layer.

This project explores those ideas in a practical codebase.

## Current status

The project is an active implementation foundation. It has real control-plane structure and several cloud-resource areas underway, but it is not production-grade infrastructure yet.

Strongest current areas include:

- instance lifecycle foundations
- async job handling
- reconciliation and repair concepts
- networking foundations
- volume and image workflows
- host-agent and scheduler seams

Ongoing areas include:

- stronger tenancy and quotas
- deeper image trust and validation
- more complete fleet operations
- live hardware validation
- console and developer experience polish

## Block storage direction

The platform includes work toward an EBS-style block storage subsystem, including volumes, attachments, snapshots, restore workflows, performance tiers, encryption, quotas, and observability concepts.

This is an active architecture and implementation area and should be viewed as a foundation rather than a production storage service.

## Development

Common Go validation commands:

```bash
go build ./...
go test ./...
go vet ./...
```

Some integration tests may require external services such as a database or a Linux host environment.

## Disclaimer

This is an independent experimental infrastructure project. It is not affiliated with any cloud provider and should not be treated as production infrastructure without further hardening and validation.

## License

Add project license information here.
