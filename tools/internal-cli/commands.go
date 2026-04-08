package main

// commands.go — Internal CLI command implementations.
//
// M1 command (available now):
//   issue-bootstrap-token --host-id=<id>
//
// M2 commands:
//   create-instance  Enqueue an INSTANCE_CREATE job and wait for RUNNING.
//   delete-instance  Enqueue an INSTANCE_DELETE job and wait for DELETED.
//
// Source: IMPLEMENTATION_PLAN_V1 §40 (internal CLI for M2 vertical slice).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// ── issue-bootstrap-token ─────────────────────────────────────────────────────

// cmdIssueBootstrapToken creates a one-time bootstrap token for a new host.
//
// Usage: internal-cli issue-bootstrap-token --host-id=<id>
//
// Workflow:
//  1. Connect to PostgreSQL via DATABASE_URL.
//  2. Generate a cryptographically random 32-byte token.
//  3. Store SHA-256(token) in bootstrap_tokens with 1h expiry.
//  4. Print the raw token to stdout — shown exactly once, never stored.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §6, IMPLEMENTATION_PLAN_V1 §M1 gaps.
func cmdIssueBootstrapToken(args []string) error {
	hostID := flagValue(args, "--host-id")
	if hostID == "" {
		return fmt.Errorf("--host-id is required\nUsage: internal-cli issue-bootstrap-token --host-id=<host-id>")
	}

	pool, err := db.NewSQLPool(db.DatabaseURL())
	if err != nil {
		return fmt.Errorf("database connection failed: %w", err)
	}
	defer pool.Close()
	repo := db.New(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rawToken, err := doIssueToken(ctx, repo, hostID)
	if err != nil {
		return fmt.Errorf("issue-bootstrap-token: %w", err)
	}

	out := struct {
		HostID    string `json:"host_id"`
		Token     string `json:"token"`
		ExpiresIn string `json:"expires_in"`
		Note      string `json:"note"`
	}{
		HostID:    hostID,
		Token:     rawToken,
		ExpiresIn: "1h",
		Note:      "Write this token to /etc/host-agent/bootstrap-token on the host before starting the agent. It is shown once only.",
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func doIssueToken(ctx context.Context, repo *db.Repo, hostID string) (string, error) {
	rawToken, err := generateToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	tokenHash := sha256hex(rawToken)
	expiresAt := time.Now().UTC().Add(time.Hour)
	if err := repo.InsertBootstrapToken(ctx, tokenHash, hostID, expiresAt); err != nil {
		return "", fmt.Errorf("insert token: %w", err)
	}
	return rawToken, nil
}

// ── create-instance ───────────────────────────────────────────────────────────

// cmdCreateInstance enqueues an INSTANCE_CREATE job and polls until the instance
// reaches RUNNING state or times out.
//
// Usage: internal-cli create-instance \
//   --name=my-vm \
//   --image-id=00000000-0000-0000-0000-000000000010 \
//   --instance-type=c1.small \
//   --az=us-east-1a \
//   --principal-id=<UUID> \
//   [--ssh-key="ssh-ed25519 AAAA..."] \
//   [--timeout=300]
//
// Source: IMPLEMENTATION_PLAN_V1 §40.
func cmdCreateInstance(args []string) error {
	name         := flagValue(args, "--name")
	imageID      := flagValue(args, "--image-id")
	instanceType := flagValue(args, "--instance-type")
	az           := flagValue(args, "--az")
	principalID  := flagValue(args, "--principal-id")
	sshKey       := flagValue(args, "--ssh-key")
	timeoutStr   := flagValue(args, "--timeout")

	// Defaults.
	if name == ""         { name = "m2-test-vm" }
	if imageID == ""      { imageID = "00000000-0000-0000-0000-000000000010" } // ubuntu-22.04
	if instanceType == "" { instanceType = "c1.small" }
	if az == ""           { az = "us-east-1a" }
	if principalID == ""  { principalID = "00000000-0000-0000-0000-000000000001" } // system principal
	timeout := 300 * time.Second
	if timeoutStr != "" {
		if n, err := time.ParseDuration(timeoutStr + "s"); err == nil {
			timeout = n
		}
	}

	pool, err := db.NewSQLPool(db.DatabaseURL())
	if err != nil {
		return fmt.Errorf("database connection failed: %w", err)
	}
	defer pool.Close()
	repo := db.New(pool)

	ctx, cancel := context.WithTimeout(context.Background(), timeout+30*time.Second)
	defer cancel()

	instanceID := idgen.New(idgen.PrefixInstance)
	jobID       := idgen.New(idgen.PrefixJob)
	idemKey     := "create-" + instanceID

	// Insert instance record in requested state.
	if err := repo.InsertInstance(ctx, &db.InstanceRow{
		ID:               instanceID,
		Name:             name,
		OwnerPrincipalID: principalID,
		InstanceTypeID:   instanceType,
		ImageID:          imageID,
		AvailabilityZone: az,
	}); err != nil {
		return fmt.Errorf("insert instance: %w", err)
	}

	// Enqueue INSTANCE_CREATE job.
	if err := repo.InsertJob(ctx, &db.JobRow{
		ID:             jobID,
		InstanceID:     instanceID,
		JobType:        "INSTANCE_CREATE",
		IdempotencyKey: idemKey,
		MaxAttempts:    3,
	}); err != nil {
		return fmt.Errorf("insert job: %w", err)
	}

	fmt.Printf("instance_id: %s\njob_id:      %s\n\n", instanceID, jobID)
	if sshKey != "" {
		fmt.Printf("ssh_key:     %s\n\n", sshKey[:min(len(sshKey), 40)]+"...")
	}
	fmt.Println("Waiting for instance to reach RUNNING state...")

	// Poll until running or failed.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)

		inst, err := repo.GetInstanceByID(ctx, instanceID)
		if err != nil {
			fmt.Printf("  [poll] error reading instance: %v\n", err)
			continue
		}
		fmt.Printf("  [poll] state=%s\n", inst.VMState)

		switch inst.VMState {
		case "running":
			ip, _ := repo.GetIPByInstance(ctx, instanceID)
			fmt.Printf("\n✓ Instance RUNNING\n")
			fmt.Printf("  instance_id: %s\n", instanceID)
			fmt.Printf("  private_ip:  %s\n", ip)
			if sshKey != "" {
				fmt.Printf("  ssh:         ssh root@%s\n", ip)
			}
			return nil
		case "failed":
			return fmt.Errorf("instance failed during provisioning — check instance_events for details")
		}
	}
	return fmt.Errorf("timeout: instance did not reach RUNNING within %s", timeout)
}

// ── delete-instance ───────────────────────────────────────────────────────────

// cmdDeleteInstance enqueues an INSTANCE_DELETE job and polls until the instance
// reaches DELETED state or times out.
//
// Usage: internal-cli delete-instance --instance-id=<id> [--timeout=120]
//
// Source: IMPLEMENTATION_PLAN_V1 §40.
func cmdDeleteInstance(args []string) error {
	instanceID := flagValue(args, "--instance-id")
	timeoutStr := flagValue(args, "--timeout")

	if instanceID == "" {
		return fmt.Errorf("--instance-id is required\nUsage: internal-cli delete-instance --instance-id=<id>")
	}
	timeout := 120 * time.Second
	if timeoutStr != "" {
		if n, err := time.ParseDuration(timeoutStr + "s"); err == nil {
			timeout = n
		}
	}

	pool, err := db.NewSQLPool(db.DatabaseURL())
	if err != nil {
		return fmt.Errorf("database connection failed: %w", err)
	}
	defer pool.Close()
	repo := db.New(pool)

	ctx, cancel := context.WithTimeout(context.Background(), timeout+30*time.Second)
	defer cancel()

	// Verify the instance exists and is in a deletable state.
	inst, err := repo.GetInstanceByID(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("instance not found: %w", err)
	}
	if inst.VMState == "deleted" {
		fmt.Printf("Instance %s is already deleted.\n", instanceID)
		return nil
	}

	jobID   := idgen.New(idgen.PrefixJob)
	idemKey := "delete-" + instanceID

	if err := repo.InsertJob(ctx, &db.JobRow{
		ID:             jobID,
		InstanceID:     instanceID,
		JobType:        "INSTANCE_DELETE",
		IdempotencyKey: idemKey,
		MaxAttempts:    3,
	}); err != nil {
		// Duplicate idempotency key means a delete job is already enqueued — that's fine.
		if !strings.Contains(err.Error(), "duplicate") {
			return fmt.Errorf("insert delete job: %w", err)
		}
		fmt.Println("Delete job already enqueued — monitoring progress...")
	} else {
		fmt.Printf("instance_id: %s\njob_id:      %s\n\n", instanceID, jobID)
		fmt.Println("Waiting for instance to reach DELETED state...")
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)

		// Use a raw query here — GetInstanceByID filters deleted_at IS NULL.
		// After soft-delete the instance has deleted_at set; query directly.
		var state string
		err := pool.QueryRow(ctx,
			"SELECT vm_state FROM instances WHERE id = $1", instanceID,
		).Scan(&state)
		if err != nil {
			fmt.Printf("  [poll] error: %v\n", err)
			continue
		}
		fmt.Printf("  [poll] state=%s\n", state)

		if state == "deleted" {
			fmt.Printf("\n✓ Instance DELETED\n")
			fmt.Printf("  instance_id: %s\n", instanceID)
			return nil
		}
	}
	return fmt.Errorf("timeout: instance did not reach DELETED within %s", timeout)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func flagValue(args []string, flag string) string {
	prefix := flag + "="
	for _, a := range args {
		if len(a) > len(prefix) && a[:len(prefix)] == prefix {
			return a[len(prefix):]
		}
	}
	return ""
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
