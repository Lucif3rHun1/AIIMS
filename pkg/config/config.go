package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

const DefaultTimezone = "Asia/Kolkata"

type Config struct {
	AuthToken    string `json:"auth_token"`
	RefreshToken string `json:"refresh_token"`
	HipID        string `json:"hip_id"`
	TargetDate   string `json:"target_date"`
	Timezone     string `json:"timezone,omitempty"`

	TelegramBotToken string   `json:"telegram_bot_token"`
	OwnerID          int64    `json:"owner_id"`
	BroadcastChatID  int64    `json:"broadcast_chat_id"`
	AuthorizedUsers  []string `json:"authorized_users"`
}

func (c *Config) Validate() error {
	if c.AuthToken == "" {
		return fmt.Errorf("auth_token is required")
	}
	if c.RefreshToken == "" {
		return fmt.Errorf("refresh_token is required")
	}
	if c.HipID == "" {
		return fmt.Errorf("hip_id is required")
	}
	if c.Timezone == "" {
		c.Timezone = DefaultTimezone
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("invalid timezone '%s': %v", c.Timezone, err)
	}
	if c.TargetDate != "" {
		if _, err := time.Parse("2006-01-02", c.TargetDate); err != nil {
			return fmt.Errorf("invalid target_date format (expected YYYY-MM-DD): %v", err)
		}
	}
	if c.TelegramBotToken == "" {
		return fmt.Errorf("telegram_bot_token is required")
	}
	if c.OwnerID == 0 {
		return fmt.Errorf("owner_id is required")
	}
	return nil
}

func (c *Config) IsOwner(userID int64) bool {
	return userID == c.OwnerID
}

func (c *Config) HasBroadcastChannel() bool {
	return c.BroadcastChatID != 0
}

func (c *Config) SetBroadcastChatID(chatID int64) {
	c.BroadcastChatID = chatID
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %v", err)
	}
	return cfg, nil
}

func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}
	return nil
}

func (c *Config) Redact() string {
	mask := func(s string) string {
		if len(s) <= 8 {
			return "****"
		}
		return s[:4] + "..." + s[len(s)-4:]
	}
	return fmt.Sprintf("Config{HIP:%s Date:%s TZ:%s Bot:%s Owner:%d Broadcast:%d}",
		c.HipID, c.TargetDate, c.Timezone,
		mask(c.TelegramBotToken), c.OwnerID, c.BroadcastChatID,
	)
}
