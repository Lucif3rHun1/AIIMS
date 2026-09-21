package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"aiims-appointment/pkg/abdm"
	"aiims-appointment/pkg/config"
)

func accountKey(phone string) string { return config.AccountKey(phone) }

// TokenRefresher manages proactive token refresh for all accounts.
// It runs as a background goroutine, periodically checking token expiry
// and refreshing tokens before they expire to avoid OTP prompts.
type TokenRefresher struct {
	config   *config.Config
	interval time.Duration
	stopCh   chan struct{}
	running  atomic.Bool
}

// NewTokenRefresher creates a new token refresher.
// Default check interval is 5 minutes.
func NewTokenRefresher(cfg *config.Config) *TokenRefresher {
	return &TokenRefresher{
		config:   cfg,
		interval: 5 * time.Minute,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the background token refresh loop.
func (tr *TokenRefresher) Start() {
	if !tr.running.CompareAndSwap(false, true) {
		return
	}
	go tr.loop()
	slog.Info("token refresher started", "component", "refresher", "interval", tr.interval)
}

// Stop halts the background token refresh loop.
func (tr *TokenRefresher) Stop() {
	// CompareAndSwap, not a plain bool: two Stop() calls used to close stopCh
	// twice, and a double close panics.
	if !tr.running.CompareAndSwap(true, false) {
		return
	}
	close(tr.stopCh)
	slog.Info("token refresher stopped", "component", "refresher")
}

// RefreshAll immediately refreshes tokens for all accounts that need it.
// Returns a summary of refresh results.
func (tr *TokenRefresher) RefreshAll() RefreshSummary {
	var summary RefreshSummary
	accounts := tr.config.AccountList()

	for _, acc := range accounts {
		// Skip accounts without tokens
		if acc.AuthToken == "" || acc.RefreshToken == "" {
			summary.Skipped++
			continue
		}

		// Check token expiry
		expiry := abdm.ParseJWTExpiry(acc.AuthToken)
		if expiry.IsZero() {
			// Unparseable token. The usual cause is a wrong or rotated
			// AIIMS_MASTER_KEY leaving ciphertext in the field, which silently
			// stops every refresh for this account until someone notices.
			slog.Warn("token expiry unreadable, skipping refresh",
				"component", "refresher", "account_key", accountKey(acc.PhoneNumber))
			summary.Skipped++
			continue
		}

		// Refresh if token expires within 10 minutes
		refreshThreshold := 10 * time.Minute
		if time.Until(expiry) > refreshThreshold {
			summary.Healthy++
			continue
		}

		// Token needs refresh
		key := accountKey(acc.PhoneNumber)
		slog.Info("proactive token refresh", "component", "refresher", "account_key", key, "expires_in", time.Until(expiry).Round(time.Second))
		if err := tr.refreshAccount(acc); err != nil {
			summary.Failed++
			summary.Errors = append(summary.Errors, fmt.Sprintf("%s: %v", key, err))
			slog.Error("proactive refresh failed", "component", "refresher", "account_key", key, "error", err)
		} else {
			summary.Refreshed++
			slog.Info("proactive refresh succeeded", "component", "refresher", "account_key", key)
		}
	}

	return summary
}

// refreshAccount refreshes tokens for a single account.
func (tr *TokenRefresher) refreshAccount(acc *config.Account) error {
	// Create a temporary manager just for token refresh
	cfg := acc.CloneForABDM(tr.config)

	// Create HTTP client and token manager
	httpClient := abdm.NewHTTPClient()
	deviceID, _ := abdm.GenerateSecureUUID()
	masterToken := &abdm.Token{
		Auth:      cfg.AuthToken,
		Sess:      cfg.AuthToken,
		Refresh:   cfg.RefreshToken,
		DeviceID:  deviceID,
		ExpiresAt: abdm.ParseJWTExpiry(cfg.AuthToken),
	}

	tm := abdm.NewTokenManager(cfg, httpClient, masterToken, context.Background())

	if err := tm.RefreshMasterToken(); err != nil {
		return fmt.Errorf("refresh failed: %w", err)
	}

	// Tokens are updated in the cloned config via writeback,
	// which propagates to the original account and saves to disk
	return nil
}

// loop is the main refresh loop.
func (tr *TokenRefresher) loop() {
	// Do an immediate refresh on startup
	summary := tr.RefreshAll()
	if summary.Total() > 0 {
		slog.Info("initial refresh complete", "component", "refresher",
			"refreshed", summary.Refreshed,
			"failed", summary.Failed,
			"healthy", summary.Healthy,
			"skipped", summary.Skipped)
	}

	ticker := time.NewTicker(tr.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			summary := tr.RefreshAll()
			if summary.Total() > 0 {
				slog.Info("periodic refresh complete", "component", "refresher",
					"refreshed", summary.Refreshed,
					"failed", summary.Failed,
					"healthy", summary.Healthy,
					"skipped", summary.Skipped)
			}
		case <-tr.stopCh:
			return
		}
	}
}

// RefreshSummary holds the results of a refresh operation.
type RefreshSummary struct {
	Refreshed int
	Failed    int
	Healthy   int
	Skipped   int
	Errors    []string
}

// Total returns the total number of accounts checked.
func (s RefreshSummary) Total() int {
	return s.Refreshed + s.Failed + s.Healthy + s.Skipped
}

// NeedsAttention returns true if any accounts failed refresh.
func (s RefreshSummary) NeedsAttention() bool {
	return s.Failed > 0
}
