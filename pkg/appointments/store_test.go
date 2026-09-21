package appointments

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Smoke test: open an in-memory SQLite, upsert, query LiveToday, delete temp file.
// NOTE: modernc.org/sqlite supports ":memory:" via the special DSN.
func TestStore_UpsertAndLiveToday(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "smoke.db")
	t.Cleanup(func() { _ = os.Remove(path) })

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	now := time.Date(2026, 7, 13, 6, 0, 0, 0, time.UTC)

	rec := Record{
		HealthID:       "HID-1",
		TargetDateIST:  "2026-07-13",
		HipID:          "HIP-AIIMS",
		PatientName:    "Test Patient",
		HipName:        "AIIMS Raipur",
		TokenNumber:    "T-42",
		ConfirmedAtUTC: now,
	}
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.LiveToday(ctx, "2026-07-13")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].PatientName != "Test Patient" || got[0].TokenNumber != "T-42" {
		t.Fatalf("row mismatch: %+v", got[0])
	}

	// Idempotency: re-upsert with different token should update, not insert.
	rec.TokenNumber = "T-43"
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("upsert #2: %v", err)
	}
	got2, err := s.LiveToday(ctx, "2026-07-13")
	if err != nil {
		t.Fatalf("query #2: %v", err)
	}
	if len(got2) != 1 || got2[0].TokenNumber != "T-43" {
		t.Fatalf("dedup update failed: %+v", got2)
	}

	// Different target_date must NOT match.
	got3, err := s.LiveToday(ctx, "2026-07-14")
	if err != nil {
		t.Fatalf("query #3: %v", err)
	}
	if len(got3) != 0 {
		t.Fatalf("expected empty for different date, got %d rows", len(got3))
	}
}

// In-memory smoke variant — proves the ":memory:" path works without a file.
func TestStore_InMemory(t *testing.T) {
	// ":memory:" is interpreted by modernc.org/sqlite as a private in-memory db.
	// Open appends journal/busy pragmas; that is fine for in-memory.
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open in-memory: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	if err := s.Upsert(ctx, Record{
		HealthID:       "HID-X",
		TargetDateIST:  "2026-07-13",
		HipID:          "HIP-X",
		PatientName:    "InMemory",
		HipName:        "X",
		TokenNumber:    "T-99",
		ConfirmedAtUTC: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.LiveToday(ctx, "2026-07-13")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
}
