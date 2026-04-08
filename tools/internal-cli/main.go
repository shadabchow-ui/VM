package main

// main.go — Internal CLI tool for platform operators.
//
// M1 commands:
//   issue-bootstrap-token --host-id=<id>
//
// M2 commands:
//   create-instance   Trigger INSTANCE_CREATE job (Internal CreateVM vertical slice).
//   delete-instance   Trigger INSTANCE_DELETE job.
//
// Source: IMPLEMENTATION_PLAN_V1 §40, §M1 gaps.
//
// Usage:
//   export DATABASE_URL="postgres://..."
//   export NETWORK_CONTROLLER_URL="http://network-controller.internal:8083"
//   ./internal-cli issue-bootstrap-token --host-id=host-abc123
//   ./internal-cli create-instance --name=my-vm --ssh-key="ssh-ed25519 AAAA..."
//   ./internal-cli delete-instance --instance-id=inst_abc123

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd  := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "issue-bootstrap-token":
		err = cmdIssueBootstrapToken(args)
	case "create-instance":
		err = cmdCreateInstance(args)
	case "delete-instance":
		err = cmdDeleteInstance(args)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", cmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `internal-cli — Compute Platform Operator Tool

COMMANDS

  M1:
    issue-bootstrap-token --host-id=<id>
        Generate a one-time bootstrap token for a new host agent.
        Requires DATABASE_URL. Token is printed once and must be placed at
        /etc/host-agent/bootstrap-token on the target host.

  M2:
    create-instance [flags]
        Enqueue INSTANCE_CREATE job and wait for RUNNING state.
        Flags:
          --name=<name>            VM name (default: m2-test-vm)
          --image-id=<uuid>        Base image UUID (default: ubuntu-22.04)
          --instance-type=<type>   Instance type (default: c1.small)
          --az=<az>                Availability zone (default: us-east-1a)
          --principal-id=<uuid>    Owner principal UUID (default: system)
          --ssh-key="ssh-ed25519 AAAA..."  SSH public key to inject
          --timeout=<seconds>      Wait timeout in seconds (default: 300)

    delete-instance --instance-id=<id> [--timeout=<seconds>]
        Enqueue INSTANCE_DELETE job and wait for DELETED state.

ENVIRONMENT
    DATABASE_URL             PostgreSQL connection string (required for all commands).
    NETWORK_CONTROLLER_URL   Network controller URL (used by worker, not CLI directly).

SOURCE
    IMPLEMENTATION_PLAN_V1 §40, §M1 gaps.
`)
}
