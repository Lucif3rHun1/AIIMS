package config

import (
	"aiims-appointment/pkg/crypto"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const DefaultTimezone = "Asia/Kolkata"

type Config struct {
	mu sync.RWMutex `json:"-"`

	AuthToken    string `json:"auth_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	HipID        string `json:"hip_id,omitempty"`
	TargetDate   string `json:"target_date,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	PhoneNumber  string `json:"phone_number,omitempty"`

	TelegramBotToken string   `json:"telegram_bot_token"`
	OwnerID          int64    `json:"owner_id"`
	BroadcastChatID  int64    `json:"broadcast_chat_id"`
	AuthorizedUsers  []string `json:"authorized_users,omitempty"`

	Accounts        map[string]*Account `json:"accounts,omitempty"`
	ActiveAccountID string              `json:"active_account_id,omitempty"`

	// SavePath is set by LoadConfig and used by cloned configs for persistence.
	// Not serialized to JSON.
	SavePath string `json:"-"`

	// tokenWriteback is set by CloneForABDM to propagate token updates back
	// to the original global config and persist to disk. Not serialized.
	tokenWriteback func(string, string) `json:"-"`
}

func (c *Config) Validate() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.TelegramBotToken == "" {
		return fmt.Errorf("telegram_bot_token is required")
	}
	if c.OwnerID == 0 {
		return fmt.Errorf("owner_id is required")
	}
	if c.Timezone == "" {
		c.Timezone = DefaultTimezone
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("invalid timezone '%s': %v", c.Timezone, err)
	}
	return nil
}

func (c *Config) IsOwner(userID int64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return userID == c.OwnerID
}

func (c *Config) HasBroadcastChannel() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BroadcastChatID != 0
}

func (c *Config) SetBroadcastChatID(chatID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.BroadcastChatID = chatID
}

func (c *Config) GetActiveAccount() *Account {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.ActiveAccountID == "" {
		return nil
	}
	return c.Accounts[c.ActiveAccountID]
}

func (c *Config) SetActiveAccount(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Accounts[id] == nil {
		return false
	}
	c.ActiveAccountID = id
	return true
}

func (c *Config) AddAccount(acc *Account) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Accounts == nil {
		c.Accounts = make(map[string]*Account)
	}
	c.Accounts[acc.ID] = acc
	if c.ActiveAccountID == "" {
		c.ActiveAccountID = acc.ID
	}
}

func (c *Config) RemoveAccount(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Accounts[id] == nil {
		return false
	}
	delete(c.Accounts, id)
	if c.ActiveAccountID == id {
		c.ActiveAccountID = ""
		for k := range c.Accounts {
			c.ActiveAccountID = k
			break
		}
	}
	return true
}

func (c *Config) RenameAccount(id, newName string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	acc := c.Accounts[id]
	if acc == nil {
		return false
	}
	acc.Name = newName
	return true
}

func (c *Config) AccountList() []*Account {
	c.mu.RLock()
	defer c.mu.RUnlock()
	list := make([]*Account, 0, len(c.Accounts))
	for _, acc := range c.Accounts {
		list = append(list, acc)
	}
	return list
}

func (c *Config) MigrateLegacyAccount() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.PhoneNumber == "" {
		return
	}
	if c.Accounts == nil {
		c.Accounts = make(map[string]*Account)
	}
	if c.Accounts[c.PhoneNumber] != nil {
		return
	}
	acc := &Account{
		ID:           c.PhoneNumber,
		Name:         c.PhoneNumber,
		PhoneNumber:  c.PhoneNumber,
		AuthToken:    c.AuthToken,
		RefreshToken: c.RefreshToken,
		TargetDate:   c.TargetDate,
		SelectedIDs:  make(map[string]bool),
		CreatedAt:    time.Now(),
	}
	c.Accounts[c.PhoneNumber] = acc
	if c.ActiveAccountID == "" {
		c.ActiveAccountID = c.PhoneNumber
	}
}

func (c *Config) UpdateTokens(sess, refresh string) {
	c.mu.Lock()
	c.AuthToken = sess
	c.RefreshToken = refresh
	if c.ActiveAccountID != "" {
		if acc := c.Accounts[c.ActiveAccountID]; acc != nil {
			acc.UpdateTokens(sess, refresh)
		}
	}
	c.mu.Unlock()
	if c.tokenWriteback != nil {
		c.tokenWriteback(sess, refresh)
	}
}

func (c *Config) UpdateTokensForAccount(accountID, sess, refresh string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if acc := c.Accounts[accountID]; acc != nil {
		acc.UpdateTokens(sess, refresh)
	}
}

// Clone creates a deep copy of the Config for safe serialization without
// holding the lock during I/O or json marshaling.
func (c *Config) Clone() *Config {
	c.mu.RLock()
	defer c.mu.RUnlock()

	clone := &Config{
		AuthToken:        c.AuthToken,
		RefreshToken:     c.RefreshToken,
		HipID:            c.HipID,
		TargetDate:       c.TargetDate,
		Timezone:         c.Timezone,
		PhoneNumber:      c.PhoneNumber,
		TelegramBotToken: c.TelegramBotToken,
		OwnerID:          c.OwnerID,
		BroadcastChatID:  c.BroadcastChatID,
		ActiveAccountID:  c.ActiveAccountID,
		SavePath:         c.SavePath,
	}
	if len(c.AuthorizedUsers) > 0 {
		clone.AuthorizedUsers = make([]string, len(c.AuthorizedUsers))
		copy(clone.AuthorizedUsers, c.AuthorizedUsers)
	}
	if len(c.Accounts) > 0 {
		clone.Accounts = make(map[string]*Account, len(c.Accounts))
		for k, v := range c.Accounts {
			clone.Accounts[k] = v.Clone()
		}
	}
	return clone
}

func (c *Config) Save(path string) error {
	clone := c.Clone()

	// Encrypt tokens at rest if master key is configured
	clone.encryptTokens()
	defer clone.decryptTokens()

	data, err := json.MarshalIndent(clone, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmpFile, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("failed to create temp config file: %v", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write temp config file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp config file: %v", err)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to chmod temp config file: %v", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename config file: %v", err)
	}
	return nil
}

// encryptTokens encrypts all sensitive token fields in-place for disk storage.
func (c *Config) encryptTokens() {
	if !crypto.Enabled() {
		return
	}
	var err error
	if c.AuthToken != "" {
		if c.AuthToken, err = crypto.Encrypt(c.AuthToken); err != nil {
			slog.Error("encrypt auth_token failed", "component", "config", "error", err)
		}
	}
	if c.RefreshToken != "" {
		if c.RefreshToken, err = crypto.Encrypt(c.RefreshToken); err != nil {
			slog.Error("encrypt refresh_token failed", "component", "config", "error", err)
		}
	}
	for _, acc := range c.Accounts {
		acc.encryptTokens()
	}
}

// decryptTokens restores plaintext tokens after encrypted serialization.
func (c *Config) decryptTokens() {
	if !crypto.Enabled() {
		return
	}
	var err error
	if c.AuthToken, err = crypto.Decrypt(c.AuthToken); err != nil {
		slog.Error("decrypt auth_token failed", "component", "config", "error", err)
	}
	if c.RefreshToken, err = crypto.Decrypt(c.RefreshToken); err != nil {
		slog.Error("decrypt refresh_token failed", "component", "config", "error", err)
	}
	for _, acc := range c.Accounts {
		acc.decryptTokens()
	}
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("config file is empty: %s", path)
	}

	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config (corrupted?): %v", err)
	}

	cfg.decryptTokens()

	if cfg.TelegramBotToken == "" {
		slog.Warn("config loaded with empty telegram_bot_token", "component", "config", "path", path)
	}
	if cfg.OwnerID == 0 {
		slog.Warn("config loaded with empty owner_id", "component", "config", "path", path)
	}

	cfg.MigrateLegacyAccount()
	cfg.SavePath = path

	return cfg, nil
}

func (c *Config) Redact() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	mask := func(s string) string {
		if len(s) <= 8 {
			return "****"
		}
		return s[:4] + "..." + s[len(s)-4:]
	}
	active := c.ActiveAccountID
	if active == "" {
		active = "none"
	}
	return fmt.Sprintf("Config{Accounts:%d Active:%s Bot:%s Owner:%d Broadcast:%d Hip:%s}",
		len(c.Accounts), active,
		mask(c.TelegramBotToken), c.OwnerID, c.BroadcastChatID,
		c.HipID,
	)
}
