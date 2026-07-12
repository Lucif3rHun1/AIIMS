package bot

import (
	"sync"
	"testing"
)

// ============================================================
// TestRunnerRegistryLifecycle covers:
//   - Create registry
//   - Add 3 runners with different accountIDs
//   - Verify count=3, anyRunning=false
//   - Get each runner by ID
//   - Remove one, verify count=2
//   - StopAll, verify no error
//   - Get removed runner, verify nil
// ============================================================

func TestRunnerRegistryLifecycle(t *testing.T) {
	rr := newRunnerRegistry()

	// Add 3 runners
	r1 := &accountRunner{accountID: "acc-001", accountName: "Account One"}
	r2 := &accountRunner{accountID: "acc-002", accountName: "Account Two"}
	r3 := &accountRunner{accountID: "acc-003", accountName: "Account Three"}

	rr.add(r1)
	rr.add(r2)
	rr.add(r3)

	if got := rr.count(); got != 3 {
		t.Fatalf("after adding 3 runners, count = %d, want 3", got)
	}

	if rr.anyRunning() {
		t.Fatal("anyRunning = true, want false (none actually started)")
	}

	// Verify each runner is retrievable
	for _, id := range []string{"acc-001", "acc-002", "acc-003"} {
		r := rr.get(id)
		if r == nil {
			t.Errorf("get(%q) = nil, want non-nil", id)
		} else if r.accountID != id {
			t.Errorf("get(%q).accountID = %q, want %q", id, r.accountID, id)
		}
	}

	// Verify get for non-existent ID returns nil
	if rr.get("nonexistent") != nil {
		t.Error("get(nonexistent) = non-nil, want nil")
	}

	// Remove one
	rr.remove("acc-002")
	if got := rr.count(); got != 2 {
		t.Fatalf("after removing acc-002, count = %d, want 2", got)
	}
	if rr.get("acc-002") != nil {
		t.Error("get(acc-002) after removal = non-nil, want nil")
	}

	// Verify remaining still present
	if rr.get("acc-001") == nil {
		t.Error("get(acc-001) = nil, want non-nil")
	}
	if rr.get("acc-003") == nil {
		t.Error("get(acc-003) = nil, want non-nil")
	}

	// StopAll
	rr.stopAll()
	if got := rr.count(); got != 0 {
		t.Fatalf("after stopAll, count = %d, want 0", got)
	}

	// All should be nil now
	if rr.get("acc-001") != nil {
		t.Error("get(acc-001) after stopAll = non-nil, want nil")
	}
	if rr.get("acc-003") != nil {
		t.Error("get(acc-003) after stopAll = non-nil, want nil")
	}

	// all() should return empty
	if got := rr.all(); len(got) != 0 {
		t.Errorf("all() after stopAll returned %d items, want 0", len(got))
	}
}

// ============================================================
// TestRunnerRegistryConcurrent covers:
//   - Create registry
//   - Spawn 20 goroutines: half add, half remove
//   - Different accountIDs to avoid collisions
//   - Verify registry state is consistent (no panics)
//   - Compatible with -race flag
// ============================================================

func TestRunnerRegistryConcurrent(t *testing.T) {
	rr := newRunnerRegistry()
	const total = 20
	const half = total / 2

	var wg sync.WaitGroup
	wg.Add(total)

	// Half add runners: acc-000 .. acc-009
	for i := range half {
		go func(idx int) {
			defer wg.Done()
			id := "acc-concurrent-add"
			// Use unique IDs by appending index
			r := &accountRunner{
				accountID:   id,
				accountName: "Concurrent Adder",
			}
			// Use indexed suffix for uniqueness in add path
			r.accountID = "cc-" + itoa(idx)
			rr.add(r)
		}(i)
	}

	// Half remove runners: cc-10 .. cc-19 (none exist, but tests concurrent remove)
	for i := half; i < total; i++ {
		go func(idx int) {
			defer wg.Done()
			rr.remove("cc-" + itoa(idx))
		}(i)
	}

	wg.Wait()

	// Registry should be consistent — no panics is the main check
	// Count should equal number of adds that weren't removed
	finalCount := rr.count()
	if finalCount > half {
		t.Errorf("final count = %d, want <= %d (number of adds)", finalCount, half)
	}

	// Verify we can still read safely
	_ = rr.all()
	_ = rr.anyRunning()
	for i := range half {
		_ = rr.get("cc-" + itoa(i))
	}

	// Clean up
	rr.stopAll()
	if rr.count() != 0 {
		t.Errorf("after stopAll, count = %d, want 0", rr.count())
	}
}

// itoa converts a small int to string without importing strconv.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	n := i
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
