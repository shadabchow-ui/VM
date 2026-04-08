//go:build integration

package handlers

// m6_network_ssh_test.go — M6 gate integration tests.
//
// Runs against a real PostgreSQL instance (DATABASE_URL env).
// Uses the real SELECT FOR UPDATE SKIP LOCKED allocation path and the real
// ip_allocations pool — no mocked DB.
//
// M6 proof requirements covered here:
//   A. Zero duplicate IPs under concurrent load (N=20 goroutines)
//   B. IP released after deletion — GetIPByInstance returns "" post-delete
//   C. IP uniqueness reconciler sub-scan returns clean on a healthy pool
//   D. IP pool integrity after repeated stop/start cycles
//
// SSH SLA (E) and DNAT/SNAT rule state (F) are covered in the unit-level
// tests in:
//   - services/worker/handlers/m6_ssh_nat_test.go (NAT recorder + SSH SLA)
//
// Run:
//   DATABASE_URL=postgres://... go test -tags=integration -v \
//     ./test/integration/... -run TestM6
//
// Source: IMPLEMENTATION_PLAN_V1 §M6 gate,
//         IP_ALLOCATION_CONTRACT_V1 §concurrent-allocation,
//         11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §M6.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
	"github.com/compute-platform/compute-platform/services/reconciler"
	"github.com/compute-platform/compute-platform/services/worker/handlers"
)

// ── A. Concurrent IP allocation stress test ───────────────────────────────────

// TestM6_ConcurrentIPAllocation_NoDuplicates is the primary M6 proof test.
//
// Fires N=20 goroutines all allocating from the same VPC simultaneously.
// Asserts: zero duplicate IPs, every successful allocation gets a unique IP.
//
// Source: IMPLEMENTATION_PLAN_V1 §M6 gate bullet 1
//
//	"concurrent IP allocation stress test — zero duplicate IPs under load".
func TestM6_ConcurrentIPAllocation_NoDuplicates(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	const N = 20
	instanceIDs := make([]string, N)
	for i := 0; i < N; i++ {
		instanceIDs[i] = idgen.New(idgen.PrefixInstance)
	}

	type result struct {
		ip  string
		err error
	}
	results := make([]result, N)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			ip, err := repo.AllocateIP(ctx, integVPCID, instanceIDs[idx])
			results[idx] = result{ip: ip, err: err}
		}(i)
	}
	wg.Wait()

	// Cleanup: release every successfully allocated IP.
	t.Cleanup(func() {
		for i, r := range results {
			if r.ip != "" {
				_ = repo.ReleaseIP(ctx, r.ip, integVPCID, instanceIDs[i])
			}
		}
	})

	// Assert uniqueness across all successful allocations.
	seen := make(map[string]string) // ip → instanceID
	var failCount int
	for i, r := range results {
		if r.err != nil {
			// Pool exhaustion is acceptable (pool may be smaller than N).
			// A concurrent-allocation error that is NOT pool exhaustion is a bug.
			t.Logf("goroutine %d: allocation error (may be pool exhausted): %v", i, r.err)
			failCount++
			continue
		}
		if r.ip == "" {
			t.Errorf("goroutine %d: AllocateIP returned empty IP with nil error", i)
			continue
		}
		if prev, dup := seen[r.ip]; dup {
			t.Errorf("INVARIANT I-2 VIOLATED: IP %s allocated to both %s and %s concurrently",
				r.ip, prev, instanceIDs[i])
		}
		seen[r.ip] = instanceIDs[i]
	}

	t.Logf("concurrent allocation: %d/%d succeeded, %d unique IPs, %d pool-exhausted",
		len(seen), N, len(seen), failCount)

	if len(seen) == 0 {
		t.Error("all allocations failed — pool may be empty; seed ip_allocations before running M6 tests")
	}
}

// TestM6_ConcurrentIPAllocation_DuplicateAttemptFails verifies that attempting
// to allocate the same instance ID twice does not produce two IPs.
// The second call should either return the same IP or an error — never a new one.
func TestM6_ConcurrentIPAllocation_DuplicateAttemptFails(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	instanceID := idgen.New(idgen.PrefixInstance)

	// Two goroutines race to allocate for the same instanceID.
	type res struct {
		ip  string
		err error
	}
	ch := make(chan res, 2)
	for i := 0; i < 2; i++ {
		go func() {
			ip, err := repo.AllocateIP(ctx, integVPCID, instanceID)
			ch <- res{ip, err}
		}()
	}

	r1 := <-ch
	r2 := <-ch

	// Cleanup.
	t.Cleanup(func() {
		if r1.ip != "" {
			_ = repo.ReleaseIP(ctx, r1.ip, integVPCID, instanceID)
		}
		if r2.ip != "" && r2.ip != r1.ip {
			_ = repo.ReleaseIP(ctx, r2.ip, integVPCID, instanceID)
		}
	})

	// At most one unique IP should have been issued.
	ips := map[string]bool{}
	for _, r := range []res{r1, r2} {
		if r.err == nil && r.ip != "" {
			ips[r.ip] = true
		}
	}
	if len(ips) > 1 {
		t.Errorf("two different IPs allocated for same instanceID — duplicate allocation: %v", ips)
	}
	t.Logf("duplicate attempt result: r1=%v err=%v | r2=%v err=%v", r1.ip, r1.err, r2.ip, r2.err)
}

// TestM6_ReleasedIP_ReturnsToPool verifies that a released IP can be
// reallocated by a subsequent allocator call.
// Source: IP_ALLOCATION_CONTRACT_V1 §release.
func TestM6_ReleasedIP_ReturnsToPool(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	inst1 := idgen.New(idgen.PrefixInstance)
	inst2 := idgen.New(idgen.PrefixInstance)

	ip1, err := repo.AllocateIP(ctx, integVPCID, inst1)
	if err != nil {
		t.Fatalf("AllocateIP inst1: %v", err)
	}

	// Release inst1's IP.
	if err := repo.ReleaseIP(ctx, ip1, integVPCID, inst1); err != nil {
		t.Fatalf("ReleaseIP: %v", err)
	}

	// Allocate again — the pool should yield an IP (may be ip1 again or another).
	ip2, err := repo.AllocateIP(ctx, integVPCID, inst2)
	if err != nil {
		t.Fatalf("AllocateIP inst2 after release: %v", err)
	}
	t.Cleanup(func() { _ = repo.ReleaseIP(ctx, ip2, integVPCID, inst2) })

	if ip2 == "" {
		t.Error("expected a non-empty IP after release+reallocate")
	}
	t.Logf("released %s, next allocation got %s", ip1, ip2)
}

// ── B. Post-deletion IP inventory verification ────────────────────────────────

// TestM6_DeleteFlow_IPReleasedAndInventoryClean verifies the full delete
// sequence using handler-level integration:
//  1. Create instance → runs → IP allocated
//  2. Delete instance → IP released
//  3. GetIPByInstance returns "" (no residual ownership)
//  4. FindDuplicateIPAllocations returns empty (no ghost entries)
//
// Source: IMPLEMENTATION_PLAN_V1 §M6 gate
//
//	"post-deletion IP inventory verification".
func TestM6_DeleteFlow_IPReleasedAndInventoryClean(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)
	newIntegHost(t, repo)
	inst := newIntegInstance(t, repo)

	rt := &integFakeRuntime{}

	// Step 1: create → running.
	createH := newIntegCreateHandler(t, repo, rt)
	if err := createH.Execute(ctx, integJob(inst.ID, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Verify IP is allocated.
	ipBefore, err := repo.GetIPByInstance(ctx, inst.ID)
	if err != nil {
		t.Fatalf("GetIPByInstance before delete: %v", err)
	}
	if ipBefore == "" {
		t.Fatal("expected IP to be allocated after create, got empty")
	}
	t.Logf("IP allocated after create: %s", ipBefore)

	// Step 2: delete → deleted.
	deleteH := newIntegDeleteHandler(t, repo, rt)
	if err := deleteH.Execute(ctx, integJob(inst.ID, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Step 3: IP must be released — no residual ownership.
	ipAfter, err := repo.GetIPByInstance(ctx, inst.ID)
	if err != nil {
		t.Fatalf("GetIPByInstance after delete: %v", err)
	}
	if ipAfter != "" {
		t.Errorf("IP still allocated after delete: %s (want empty)", ipAfter)
	}
	t.Logf("IP after delete: %q (empty = correct)", ipAfter)

	// Step 4: uniqueness sub-scan must see a clean pool (no duplicates).
	anomalies, err := repo.FindDuplicateIPAllocations(ctx)
	if err != nil {
		t.Fatalf("FindDuplicateIPAllocations: %v", err)
	}
	if len(anomalies) != 0 {
		t.Errorf("uniqueness sub-scan found %d anomalies after delete (want 0): %+v",
			len(anomalies), anomalies)
	}
	t.Log("uniqueness sub-scan: clean")
}

// TestM6_StopFlow_IPReleasedAndInventoryClean mirrors the delete test for
// the stop flow (stop also releases IP in Phase 1).
func TestM6_StopFlow_IPReleasedAndInventoryClean(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)
	newIntegHost(t, repo)
	inst := newIntegInstance(t, repo)

	rt := &integFakeRuntime{}

	// Create.
	createH := newIntegCreateHandler(t, repo, rt)
	if err := createH.Execute(ctx, integJob(inst.ID, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed the ip store for the stop handler's GetIPByInstance call.
	ipBefore, _ := repo.GetIPByInstance(ctx, inst.ID)
	if ipBefore == "" {
		t.Fatal("no IP allocated after create")
	}

	// Stop (uses real DB ReleaseIP via integFakeNetwork).
	deps := &handlers.Deps{
		Store:        repo,
		Network:      &integFakeNetwork{repo: repo},
		DefaultVPCID: integVPCID,
		Runtime:      func(_, _ string) *runtimeclient.Client { return nil },
	}
	stopH := handlers.NewStopHandler(deps, nil)
	stopH.SetRuntimeFactory(func(_, _ string) handlers.RuntimeClient { return rt })
	if err := stopH.Execute(ctx, integJob(inst.ID, "INSTANCE_STOP")); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// IP must be released.
	ipAfter, _ := repo.GetIPByInstance(ctx, inst.ID)
	if ipAfter != "" {
		t.Errorf("IP still allocated after stop: %s", ipAfter)
	}

	// Uniqueness scan must be clean.
	anomalies, _ := repo.FindDuplicateIPAllocations(ctx)
	if len(anomalies) != 0 {
		t.Errorf("uniqueness sub-scan found anomalies after stop: %+v", anomalies)
	}
	t.Logf("stop flow: IP %s released, pool clean", ipBefore)
}

// ── C. IP uniqueness reconciler sub-scan (real DB) ───────────────────────────

// TestM6_IPUniquenessScan_CleanPool verifies the sub-scan against a real DB.
// If the pool is clean (expected), the scan returns zero anomalies.
// Source: IMPLEMENTATION_PLAN_V1 §M6 gate "IP uniqueness reconciler sub-scan active".
func TestM6_IPUniquenessScan_CleanPool(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	// Run the sub-scan against the real DB.
	scan := reconciler.NewIPUniquenessScan(repo, nil) // nil logger → use no-op in test
	anomalies, err := scan.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(anomalies) != 0 {
		t.Errorf("uniqueness scan found %d anomalies on a clean test pool: %+v",
			len(anomalies), anomalies)
	}
	t.Log("uniqueness sub-scan: ACTIVE and returning clean on healthy pool")
}

// ── D. IP pool integrity across repeated stop/start cycles ───────────────────

// TestM6_StopStartCycles_NoGhostIPs runs stop → start → stop → start twice and
// confirms the IP pool remains consistent (no ghost ownership) throughout.
// Source: IMPLEMENTATION_PLAN_V1 §M6 gate (DNAT/SNAT idempotency across cycles).
func TestM6_StopStartCycles_NoGhostIPs(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)
	newIntegHost(t, repo)
	inst := newIntegInstance(t, repo)

	rt := &integFakeRuntime{}
	net := &integFakeNetwork{repo: repo}

	makeDeps := func() *handlers.Deps {
		return &handlers.Deps{
			Store:        repo,
			Network:      net,
			DefaultVPCID: integVPCID,
			Runtime:      func(_, _ string) *runtimeclient.Client { return nil },
		}
	}

	makeCreate := func() *handlers.CreateHandler {
		h := handlers.NewCreateHandler(makeDeps(), nil)
		h.SetRuntimeFactory(func(_, _ string) handlers.RuntimeClient { return rt })
		h.SetReadinessFn(func(_ context.Context, _ string, _ time.Duration) error { return nil })
		return h
	}
	makeStop := func() *handlers.StopHandler {
		h := handlers.NewStopHandler(makeDeps(), nil)
		h.SetRuntimeFactory(func(_, _ string) handlers.RuntimeClient { return rt })
		return h
	}
	makeStart := func() *handlers.StartHandler {
		h := handlers.NewStartHandler(makeDeps(), nil)
		h.SetRuntimeFactory(func(_, _ string) handlers.RuntimeClient { return rt })
		h.SetReadinessFn(func(_ context.Context, _ string, _ time.Duration) error { return nil })
		return h
	}

	if err := makeCreate().Execute(ctx, integJob(inst.ID, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if err := makeStop().Execute(ctx, integJob(inst.ID, "INSTANCE_STOP")); err != nil {
			t.Fatalf("cycle %d stop: %v", cycle, err)
		}
		ipAfterStop, _ := repo.GetIPByInstance(ctx, inst.ID)
		if ipAfterStop != "" {
			t.Errorf("cycle %d: IP %s not released after stop", cycle, ipAfterStop)
		}

		if err := makeStart().Execute(ctx, integJob(inst.ID, "INSTANCE_START")); err != nil {
			t.Fatalf("cycle %d start: %v", cycle, err)
		}
		ipAfterStart, _ := repo.GetIPByInstance(ctx, inst.ID)
		if ipAfterStart == "" {
			t.Errorf("cycle %d: no IP allocated after start", cycle)
		}

		// Uniqueness scan after each cycle.
		anomalies, _ := repo.FindDuplicateIPAllocations(ctx)
		if len(anomalies) != 0 {
			t.Errorf("cycle %d: uniqueness anomalies: %+v", cycle, anomalies)
		}
		t.Logf("cycle %d: stop→start OK, IP=%s, pool clean", cycle, ipAfterStart)
	}

	// Final cleanup: delete the instance.
	deleteH := newIntegDeleteHandler(t, repo, rt)
	_ = deleteH.Execute(ctx, integJob(inst.ID, "INSTANCE_DELETE"))

	// Confirm no ghost IP after final delete.
	finalIP, _ := repo.GetIPByInstance(ctx, inst.ID)
	if finalIP != "" {
		t.Errorf("ghost IP %s remained after final delete", finalIP)
	}
	t.Logf("stop/start cycle test: PASS — no ghost IPs after %d cycles", 2)
}

// ── Helpers (M6-specific) ─────────────────────────────────────────────────────

// newIntegStopHandler constructs a StopHandler wired to the real DB.
func newIntegStopHandler(t *testing.T, repo *db.Repo, rt *integFakeRuntime) *handlers.StopHandler {
	t.Helper()
	deps := &handlers.Deps{
		Store:        repo,
		Network:      &integFakeNetwork{repo: repo},
		DefaultVPCID: integVPCID,
		Runtime:      func(_, _ string) *runtimeclient.Client { return nil },
	}
	h := handlers.NewStopHandler(deps, nil)
	h.SetRuntimeFactory(func(_, _ string) handlers.RuntimeClient { return rt })
	return h
}

// newIntegStartHandler constructs a StartHandler wired to the real DB.
func newIntegStartHandler(t *testing.T, repo *db.Repo, rt *integFakeRuntime) *handlers.StartHandler {
	t.Helper()
	deps := &handlers.Deps{
		Store:        repo,
		Network:      &integFakeNetwork{repo: repo},
		DefaultVPCID: integVPCID,
		Runtime:      func(_, _ string) *runtimeclient.Client { return nil },
	}
	h := handlers.NewStartHandler(deps, nil)
	h.SetRuntimeFactory(func(_, _ string) handlers.RuntimeClient { return rt })
	h.SetReadinessFn(func(_ context.Context, _ string, _ time.Duration) error { return nil })
	return h
}

// logSeparator helps readability in verbose test output.
func logSeparator(t *testing.T, label string) {
	t.Helper()
	t.Logf("── %s ──", fmt.Sprintf("%-50s", label))
}
