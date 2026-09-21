package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func testPatients(n int) []Patient {
	ps := make([]Patient, n)
	for i := range ps {
		ps[i] = Patient{OID: fmt.Sprintf("oid-%d", i), FLN: fmt.Sprintf("patient %d", i)}
	}
	return ps
}

// TestAccountConcurrentAccess hammers the Account methods from several
// goroutines while Clone/Save read the same account. Before Account grew a
// mutex this died with "fatal error: concurrent map writes".
func TestAccountConcurrentAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := &Config{TelegramBotToken: "bot", OwnerID: 1, SavePath: path}
	acc := NewAccount("9999999999")
	acc.SetPatients(testPatients(20))
	cfg.AddAccount(acc)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				switch (i + n) % 6 {
				case 0:
					acc.TogglePatientSelection(fmt.Sprintf("oid-%d", n%20))
				case 1:
					acc.SelectAllPatients()
				case 2:
					acc.ClearSelections()
				case 3:
					acc.Snapshot()
				case 4:
					acc.UpdateTokens(fmt.Sprintf("auth-%d", n), fmt.Sprintf("refresh-%d", n))
				case 5:
					acc.SetPatients(testPatients(10 + n%20))
				}
			}
		}(i)
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 40; n++ {
				cfg.Clone()
				cfg.GetAccount(acc.ID)
				cfg.AccountList()
				if err := cfg.Save(path); err != nil {
					t.Errorf("Save: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("config written during the race is unreadable: %v", err)
	}
}

// TestConcurrentSaveKeepsEveryAccountToken is the lost-update regression: two
// accounts refreshing at once must not roll each other's tokens back. Every
// round is checked on disk, because a single end-state check almost never
// catches the interleaving.
func TestConcurrentSaveKeepsEveryAccountToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := &Config{TelegramBotToken: "bot", OwnerID: 1, SavePath: path}
	ids := []string{"accA", "accB", "accC", "accD"}
	for _, id := range ids {
		acc := NewAccount(id)
		acc.UpdateTokens("init-"+id, "init-"+id)
		cfg.AddAccount(acc)
	}

	for round := 0; round < 60; round++ {
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				cfg.GetAccount(id).UpdateTokens(fmt.Sprintf("auth-%s-%d", id, round), "refresh-"+id)
				if err := cfg.Save(path); err != nil {
					t.Errorf("Save: %v", err)
				}
			}(id)
		}
		wg.Wait()

		loaded, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("round %d: LoadConfig: %v", round, err)
		}
		for _, id := range ids {
			acc := loaded.GetAccount(id)
			if acc == nil {
				t.Fatalf("round %d: account %s missing from saved config", round, id)
			}
			auth, _ := acc.Tokens()
			want := fmt.Sprintf("auth-%s-%d", id, round)
			if auth != want {
				t.Fatalf("round %d: account %s auth token = %q, want %q (lost update)", round, id, auth, want)
			}
		}
	}
}

// TestWrongMasterKeyKeepsStoredTokens: a rotated or wrong AIIMS_MASTER_KEY must
// leave the stored ciphertext alone, never blank the token out.
func TestWrongMasterKeyKeepsStoredTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	t.Setenv("AIIMS_MASTER_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	cfg := &Config{TelegramBotToken: "bot", OwnerID: 1, SavePath: path, AuthToken: "global-secret"}
	acc := NewAccount("9999999999")
	acc.UpdateTokens("account-secret", "account-refresh")
	cfg.AddAccount(acc)
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	onDisk := map[string]any{}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	stored := onDisk["accounts"].(map[string]any)["9999999999"].(map[string]any)["auth_token"].(string)
	if stored == "" || stored == "account-secret" {
		t.Fatalf("token was not encrypted at rest: %q", stored)
	}

	// Key rotated out from under us.
	t.Setenv("AIIMS_MASTER_KEY", "ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=")
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig with wrong key: %v", err)
	}
	auth, refresh := reloaded.GetAccount("9999999999").Tokens()
	if auth != stored {
		t.Errorf("auth token clobbered on bad-key load: got %q, want the stored ciphertext %q", auth, stored)
	}
	if refresh == "" {
		t.Error("refresh token blanked on bad-key load")
	}
	if reloaded.AuthToken == "" {
		t.Error("global auth token blanked on bad-key load")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(raw) {
		t.Error("config file changed during a failed-decrypt load")
	}
}

// TestAccountJSONRoundTrip guards the unexported mutex staying invisible to
// encoding/json.
func TestAccountJSONRoundTrip(t *testing.T) {
	acc := NewAccount("9999999999")
	acc.UpdateTokens("auth", "refresh")
	acc.SetPatients(testPatients(3))
	acc.TogglePatientSelection("oid-1")

	data, err := json.Marshal(acc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Account
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.PatientCount() != 3 || back.SelectedCount() != 1 {
		t.Fatalf("round trip lost data: %d patients, %d selected", back.PatientCount(), back.SelectedCount())
	}
	if auth, _ := back.Tokens(); auth != "auth" {
		t.Fatalf("round trip lost auth token: %q", auth)
	}
}

// TestSetPatientsDropsStaleSelections covers the selection-preservation branch.
func TestSetPatientsDropsStaleSelections(t *testing.T) {
	acc := NewAccount("9999999999")
	acc.SetPatients(testPatients(3))
	acc.SelectAllPatients()
	acc.SetPatients([]Patient{{OID: "oid-1"}, {OID: "oid-9"}})
	if acc.SelectedCount() != 1 {
		t.Fatalf("SelectedCount = %d, want 1", acc.SelectedCount())
	}
	if _, selected := acc.Snapshot(); !selected["oid-1"] {
		t.Fatal("surviving patient lost its selection")
	}
}
