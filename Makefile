.PHONY: lint test test-unit test-integration test-integration-m1 \
        migrate-up migrate-down proto ci build-all \
        build-worker build-host-agent build-resource-manager build-scheduler build-cli \
        build-network-controller \
        m0-gate m1-gate m2-gate \
        run-worker run-host-agent run-network-controller

# ── Linting ───────────────────────────────────────────────────────────────────
lint:
	golangci-lint run ./...

# ── Tests ─────────────────────────────────────────────────────────────────────

# Unit tests: no DB, no network, runs in CI.
test-unit:
	go test -short -count=1 ./packages/... ./internal/...

# Integration tests: require DATABASE_URL to be set and migrations applied.
# The -tags=integration flag enables the real pgxpool wiring in test/integration/pool_real.go.
test-integration:
	go test -tags=integration -count=1 -timeout=120s ./test/integration/...

# M1-specific integration tests only.
test-integration-m1:
	go test -tags=integration -count=1 -timeout=60s \
		-run "TestHost|TestAuth|TestScheduler|TestMigration|TestHeartbeat" \
		./test/integration/...

# Auth unit tests (no DB).
test-auth:
	go test -count=1 -run "TestCA" ./internal/auth/...

# All tests that can run in CI without a DB.
test: test-unit test-auth

# ── Database ──────────────────────────────────────────────────────────────────
# Apply migrations using the golang-migrate CLI (requires 'migrate' binary).
# If 'migrate' is not installed, use 'make migrate-psql' instead.
migrate-up:
	migrate -path db/migrations -database "$(DATABASE_URL)" up

migrate-down:
	migrate -path db/migrations -database "$(DATABASE_URL)" down

# Apply migrations directly via psql — no external tooling required.
# Use this when the 'migrate' binary is not installed.
# Requires: psql in PATH, DATABASE_URL set.
migrate-psql:
	psql "$(DATABASE_URL)" -f db/migrations/001_initial.up.sql
	psql "$(DATABASE_URL)" -f db/migrations/002_hosts.up.sql

# ── Proto ─────────────────────────────────────────────────────────────────────
proto:
	protoc --go_out=. --go-grpc_out=. packages/contracts/runtimev1/runtime.proto

# ── Builds ────────────────────────────────────────────────────────────────────
build-all: build-worker build-host-agent build-resource-manager build-scheduler build-cli

build-worker:
	go build -o bin/worker ./services/worker/

build-host-agent:
	go build -o bin/host-agent ./services/host-agent/

build-resource-manager:
	go build -o bin/resource-manager ./services/resource-manager/

build-scheduler:
	go build -o bin/scheduler ./services/scheduler/

build-cli:
	go build -o bin/internal-cli ./tools/internal-cli/

# ── Gate checks ───────────────────────────────────────────────────────────────

# M0 gate: run the m0-gate-check.sh script.
m0-gate:
	bash scripts/m0-gate-check.sh

# M1 gate: run the m1-gate-check.sh script.
# Requires: DATABASE_URL set, make migrate-up already run.
m1-gate:
	bash scripts/m1-gate-check.sh

# M2 gate: 60 automated checks. Operator must still run hardware gate H1-H4.
# Source: IMPLEMENTATION_PLAN_V1 §Phase 3 gate criteria.
m2-gate:
	bash scripts/m2-gate-check.sh

# Build network controller service (M2).
build-network-controller:
	go build -o bin/network-controller ./services/network-controller/

# Build all M2 binaries: worker, host-agent, network-controller, internal-cli.
build-m2: build-worker build-host-agent build-network-controller build-cli

# Run the worker locally. Requires DATABASE_URL and NETWORK_CONTROLLER_URL.
run-worker:
	go run ./services/worker/

# Run the host agent locally.
# Requires: AGENT_HOST_ID, AGENT_AZ, RESOURCE_MANAGER_URL
# Optional: RUNTIME_ADDR (default :50051), METADATA_ADDR (default 169.254.169.254:80)
#           NFS_ROOT (default /mnt/nfs/vols), KERNEL_PATH (default /opt/firecracker/vmlinux)
run-host-agent:
	go run ./services/host-agent/

# Run the network controller locally. Requires DATABASE_URL.
run-network-controller:
	go run ./services/network-controller/

# M2 smoke test via internal CLI. Requires worker + host-agent + network-controller running.
# Usage: make m2-smoke-test SSH_KEY="ssh-ed25519 AAAA..."
m2-smoke-test:
	@echo "=== M2 Smoke Test: create → running → delete → deleted ==="
	@go run ./tools/internal-cli/ create-instance \
		--name=m2-smoke --instance-type=c1.small --timeout=300 \
		$$([ -n "$(SSH_KEY)" ] && echo "--ssh-key=$(SSH_KEY)") \
		| tee /tmp/m2-smoke-create.txt
	@INST=$$(grep '^instance_id' /tmp/m2-smoke-create.txt | awk '{print $$2}'); \
	echo "Deleting $$INST ..."; \
	go run ./tools/internal-cli/ delete-instance --instance-id=$$INST
	@echo "=== M2 Smoke Test PASSED ==="


ci: lint test build-all
