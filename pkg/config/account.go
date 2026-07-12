package config

import (
	"aiims-appointment/pkg/crypto"
	"log/slog"
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

type Account struct {
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

// Clone returns a deep copy of the Account.
func (a *Account) Clone() *Account {
	if a == nil {
		return nil
	}
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
	return &Config{
		AuthToken:        a.AuthToken,
		RefreshToken:     a.RefreshToken,
		HipID:            global.HipID,
		TargetDate:       a.TargetDate,
		Timezone:         global.Timezone,
		PhoneNumber:      a.PhoneNumber,
		TelegramBotToken: global.TelegramBotToken,
		OwnerID:          global.OwnerID,
		BroadcastChatID:  global.BroadcastChatID,
		AuthorizedUsers:  global.AuthorizedUsers,
		SavePath:         global.SavePath,
		tokenWriteback: func(sess, refresh string) {
			accRef.UpdateTokens(sess, refresh)
			if err := global.Save(global.SavePath); err != nil {
				slog.Error("token writeback save failed", "component", "config", "account_id", accRef.ID, "error", err)
			}
		},
	}
}

func (a *Account) UpdateTokens(sess, refresh string) {
	a.AuthToken = sess
	a.RefreshToken = refresh
}

func (a *Account) IsValid() bool {
	return a.ID != "" && a.PhoneNumber != ""
}

func (a *Account) PatientCount() int {
	return len(a.Patients)
}

func (a *Account) SelectedCount() int {
	count := 0
	for _, v := range a.SelectedIDs {
		if v {
			count++
		}
	}
	return count
}

func (a *Account) GetSelectedPatients() []Patient {
	selected := make([]Patient, 0)
	for _, p := range a.Patients {
		if a.SelectedIDs[p.OID] {
			selected = append(selected, p)
		}
	}
	return selected
}

func (a *Account) TogglePatientSelection(oid string) bool {
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
	if a.SelectedIDs == nil {
		a.SelectedIDs = make(map[string]bool)
	}
	for _, p := range a.Patients {
		a.SelectedIDs[p.OID] = true
	}
}

func (a *Account) ClearSelections() {
	a.SelectedIDs = make(map[string]bool)
}

func (a *Account) encryptTokens() {
	if !crypto.Enabled() {
		return
	}
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
	var err error
	if a.AuthToken, err = crypto.Decrypt(a.AuthToken); err != nil {
		slog.Error("decrypt account auth_token failed", "component", "config", "account_id", a.ID, "error", err)
	}
	if a.RefreshToken, err = crypto.Decrypt(a.RefreshToken); err != nil {
		slog.Error("decrypt account refresh_token failed", "component", "config", "account_id", a.ID, "error", err)
	}
}
