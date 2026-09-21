package config

import (
	"aiims-appointment/pkg/crypto"
	"log/slog"
	"sync"
	"time"
)

type Patient struct {
	OID       string   `json:"oid"`
	FLN       string   `json:"fln"`
	HealthIDs []string `json:"health-ids"`
	ABHA      string   `json:"abha"`
}

func (p *Patient) PrimaryHealthID() string {
	if len(p.HealthIDs) > 0 {
		return p.HealthIDs[0]
	}
	return ""
}

// Account is mutated from several goroutines at once (per-update handlers, the
// token refresher, the booking runner), so every field below is guarded by mu.
// Callers outside this package must go through the methods, never the fields.
//
// Lock order is always Config.mu then Account.mu (Config.Clone takes both). No
// Account method may call back into Config while holding mu, or that inverts.
type Account struct {
	mu sync.RWMutex `json:"-"`

	ID           string          `json:"id"`
	Name         string          `json:"name"`
	PhoneNumber  string          `json:"phone_number"`
	AuthToken    string          `json:"auth_token"`
	RefreshToken string          `json:"refresh_token"`
	Patients     []Patient       `json:"patients,omitempty"`
	SelectedIDs  map[string]bool `json:"selected_ids,omitempty"`
	TargetDate   string          `json:"target_date,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

func NewAccount(phoneNumber string) *Account {
	return &Account{
		ID:          phoneNumber,
		Name:        phoneNumber,
		PhoneNumber: phoneNumber,
		SelectedIDs: make(map[string]bool),
		CreatedAt:   time.Now(),
	}
}

// Clone returns a deep copy of the Account. The clone gets a fresh mutex.
func (a *Account) Clone() *Account {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	clone := &Account{
		ID:           a.ID,
		Name:         a.Name,
		PhoneNumber:  a.PhoneNumber,
		AuthToken:    a.AuthToken,
		RefreshToken: a.RefreshToken,
		TargetDate:   a.TargetDate,
		CreatedAt:    a.CreatedAt,
	}
	if len(a.Patients) > 0 {
		clone.Patients = make([]Patient, len(a.Patients))
		copy(clone.Patients, a.Patients)
	}
	if len(a.SelectedIDs) > 0 {
		clone.SelectedIDs = make(map[string]bool, len(a.SelectedIDs))
		for k, v := range a.SelectedIDs {
			clone.SelectedIDs[k] = v
		}
	}
	return clone
}

func (a *Account) CloneForABDM(global *Config) *Config {
	accRef := a

	a.mu.RLock()
	authToken, refreshToken := a.AuthToken, a.RefreshToken
	targetDate, phoneNumber := a.TargetDate, a.PhoneNumber
	a.mu.RUnlock()

	// Account lock is released before the Config lock is taken: nesting them
	// the other way round would invert Config.Clone's order and deadlock.
	global.mu.RLock()
	clone := &Config{
		AuthToken:        authToken,
		RefreshToken:     refreshToken,
		HipID:            global.HipID,
		TargetDate:       targetDate,
		Timezone:         global.Timezone,
		PhoneNumber:      phoneNumber,
		TelegramBotToken: global.TelegramBotToken,
		OwnerID:          global.OwnerID,
		BroadcastChatID:  global.BroadcastChatID,
		AuthorizedUsers:  global.AuthorizedUsers,
		SavePath:         global.SavePath,
	}
	global.mu.RUnlock()

	clone.tokenWriteback = func(sess, refresh string) {
		accRef.UpdateTokens(sess, refresh)
		if err := global.Save(global.SavePath); err != nil {
			slog.Error("token writeback save failed", "component", "config", "account_id", accRef.ID, "error", err)
		}
	}
	return clone
}

func (a *Account) UpdateTokens(sess, refresh string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.AuthToken = sess
	a.RefreshToken = refresh
}

// Tokens returns the current auth and refresh tokens under the account lock.
func (a *Account) Tokens() (auth, refresh string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AuthToken, a.RefreshToken
}

func (a *Account) SetName(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Name = name
}

func (a *Account) IsValid() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ID != "" && a.PhoneNumber != ""
}

func (a *Account) PatientCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.Patients)
}

func (a *Account) SelectedCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	count := 0
	for _, v := range a.SelectedIDs {
		if v {
			count++
		}
	}
	return count
}

func (a *Account) GetSelectedPatients() []Patient {
	a.mu.RLock()
	defer a.mu.RUnlock()
	selected := make([]Patient, 0)
	for _, p := range a.Patients {
		if a.SelectedIDs[p.OID] {
			selected = append(selected, p)
		}
	}
	return selected
}

// Snapshot returns copies of the patient list and the selection map, so callers
// that need both together (keyboard rendering) never touch the fields directly.
func (a *Account) Snapshot() ([]Patient, map[string]bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	patients := make([]Patient, len(a.Patients))
	copy(patients, a.Patients)
	selected := make(map[string]bool, len(a.SelectedIDs))
	for k, v := range a.SelectedIDs {
		selected[k] = v
	}
	return patients, selected
}

// SetPatients replaces the patient list and drops selections whose patient is
// no longer present.
func (a *Account) SetPatients(patients []Patient) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Patients = patients
	if len(a.SelectedIDs) == 0 {
		return
	}
	existing := make(map[string]bool, len(patients))
	for _, p := range patients {
		existing[p.OID] = true
	}
	for oid := range a.SelectedIDs {
		if !existing[oid] {
			delete(a.SelectedIDs, oid)
		}
	}
}

func (a *Account) TogglePatientSelection(oid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.SelectedIDs == nil {
		a.SelectedIDs = make(map[string]bool)
	}
	if a.SelectedIDs[oid] {
		delete(a.SelectedIDs, oid)
		return false
	}
	a.SelectedIDs[oid] = true
	return true
}

func (a *Account) SelectAllPatients() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.SelectedIDs == nil {
		a.SelectedIDs = make(map[string]bool)
	}
	for _, p := range a.Patients {
		a.SelectedIDs[p.OID] = true
	}
}

func (a *Account) ClearSelections() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.SelectedIDs = make(map[string]bool)
}

func (a *Account) encryptTokens() {
	if !crypto.Enabled() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	if a.AuthToken != "" {
		if a.AuthToken, err = crypto.Encrypt(a.AuthToken); err != nil {
			slog.Error("encrypt account auth_token failed", "component", "config", "account_id", a.ID, "error", err)
		}
	}
	if a.RefreshToken != "" {
		if a.RefreshToken, err = crypto.Encrypt(a.RefreshToken); err != nil {
			slog.Error("encrypt account refresh_token failed", "component", "config", "account_id", a.ID, "error", err)
		}
	}
}

func (a *Account) decryptTokens() {
	if !crypto.Enabled() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// On failure the field keeps its ciphertext: overwriting a good token with
	// "" would destroy it on the next Save (key rotation must not lose data).
	if v, err := crypto.Decrypt(a.AuthToken); err != nil {
		slog.Error("decrypt account auth_token failed, keeping stored value", "component", "config", "account_id", a.ID, "error", err)
	} else {
		a.AuthToken = v
	}
	if v, err := crypto.Decrypt(a.RefreshToken); err != nil {
		slog.Error("decrypt account refresh_token failed, keeping stored value", "component", "config", "account_id", a.ID, "error", err)
	} else {
		a.RefreshToken = v
	}
}
