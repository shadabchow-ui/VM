#!/usr/bin/env bash
set -euo pipefail

export PATH="/opt/homebrew/opt/postgresql@16/bin:$PATH"
export DATABASE_URL="${DATABASE_URL:-postgres://postgres:test@localhost:5432/compute_test?sslmode=disable}"
export RESOURCE_MANAGER_URL="${RESOURCE_MANAGER_URL:-https://localhost:9090}"
export NETWORK_CONTROLLER_URL="${NETWORK_CONTROLLER_URL:-http://localhost:8082}"
export AGENT_HOST_ID="${AGENT_HOST_ID:-host-dev-01}"
export AGENT_HOST_IP="${AGENT_HOST_IP:-127.0.0.1}"
export AGENT_AZ="${AGENT_AZ:-us-east-1a}"
export PSQL_PAGER=off

echo "Starting resource-manager..."
(go run ./services/resource-manager > /tmp/m2-resource-manager.log 2>&1) &
RM_PID=$!

sleep 2

echo "Starting network-controller..."
(go run ./services/network-controller > /tmp/m2-network-controller.log 2>&1) &
NC_PID=$!

sleep 2

echo "Starting host-agent..."
(go run ./services/host-agent > /tmp/m2-host-agent.log 2>&1) &
HA_PID=$!

sleep 2

echo "Starting worker..."
(go run ./services/worker > /tmp/m2-worker.log 2>&1) &
WK_PID=$!

sleep 3

echo "Recent job state:"
psql "$DATABASE_URL" -c "SELECT id, job_type, status, attempt_count, error_message, claimed_at, completed_at FROM jobs ORDER BY created_at DESC LIMIT 5;"

echo
echo "PIDs:"
echo "resource-manager=$RM_PID"
echo "network-controller=$NC_PID"
echo "host-agent=$HA_PID"
echo "worker=$WK_PID"
echo
echo "Logs:"
echo "/tmp/m2-resource-manager.log"
echo "/tmp/m2-network-controller.log"
echo "/tmp/m2-host-agent.log"
echo "/tmp/m2-worker.log"
