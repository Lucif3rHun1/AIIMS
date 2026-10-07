package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// StateFile is the default path for persisting runtime bot state.
const StateFile = "bot_state.json"

// BotStateSnapshot represents the persisted runtime state of the bot.
// This enables crash recovery and auto-resume on restart.
type BotStateSnapshot struct {
	Version        string            `json:"version"`
	SavedAt        time.Time         `json:"saved_at"`
	CurrentState   string            `json:"current_state"`
	StateAccountID string            `json:"state_account_id"`
	LoginStates    map[string]string `json:"login_states"` // accountID -> phase
	OTPWaiters     []string          `json:"otp_waiters"`  // accountIDs waiting for OTP
	RunningTasks   []RunningTask     `json:"running_tasks"`
	PendingDate    bool              `json:"pending_date"`
	PendingPhone   bool              `json:"pending_phone"`
}

// RunningTask represents an in-progress booking task.
type RunningTask struct {
	AccountID   string    `json:"account_id"`
	AccountName string    `json:"account_name"`
	TargetTime  time.Time `json:"target_time"`
	StartedAt   time.Time `json:"started_at"`
}

const stateVersion = "1.0"

var stateMu sync.Mutex

// SaveState persists the current bot runtime state to disk.
func (b *BotService) SaveState() error {
	stateMu.Lock()
	defer stateMu.Unlock()

	snap := &BotStateSnapshot{
		Version:      stateVersion,
		SavedAt:      time.Now(),
		CurrentState: b.CurrentState().String(),
		LoginStates:  make(map[string]string),
		OTPWaiters:   make([]string, 0),
		RunningTasks: make([]RunningTask, 0),
	}

	b.mu.RLock()
	snap.StateAccountID = b.stateAccountID
	snap.PendingDate = b.waitingForDate
	snap.PendingPhone = b.waitingForPhone

	for accID, ls := range b.loginStates {
		snap.LoginStates[accID] = ls.phase
	}

	for accID := range b.otpWaiters {
		snap.OTPWaiters = append(snap.OTPWaiters, accID)
	}
	b.mu.RUnlock()

	// Capture running tasks from registry. Membership is the liveness signal:
	// runAccount adds before start and removes on exit, and its start-of-run
	// checkpoint fires before runner.start flips running=true.
	for _, r := range b.registry.all() {
		snap.RunningTasks = append(snap.RunningTasks, RunningTask{
			AccountID:   r.accountID,
			AccountName: r.accountName,
			TargetTime:  r.target,
			StartedAt:   r.startedAt,
		})
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmpPath := StateFile + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("write state tmp: %w", err)
	}

	if err := os.Rename(tmpPath, StateFile); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename state file: %w", err)
	}

	slog.Info("bot state saved", "component", "state", "path", StateFile, "state", snap.CurrentState)
	return nil
}

// LoadState restores bot runtime state from disk.
// Returns nil if no state file exists (fresh start).
func (b *BotService) LoadState() (*BotStateSnapshot, error) {
	stateMu.Lock()
	defer stateMu.Unlock()

	data, err := os.ReadFile(StateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state file: %w", err)
	}

	var snap BotStateSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}

	if snap.Version != stateVersion {
		slog.Warn("state file version mismatch, ignoring", "component", "state", "file_version", snap.Version, "expected", stateVersion)
		return nil, nil
	}

	// State older than 30 minutes is considered stale
	if time.Since(snap.SavedAt) > 30*time.Minute {
		slog.Warn("state file is stale (>30min), ignoring", "component", "state", "saved_at", snap.SavedAt)
		return nil, nil
	}

	slog.Info("bot state loaded", "component", "state", "state", snap.CurrentState, "saved_at", snap.SavedAt)
	return &snap, nil
}

// ClearStateFile removes the persisted state file.
func (b *BotService) ClearStateFile() {
	stateMu.Lock()
	defer stateMu.Unlock()
	if err := os.Remove(StateFile); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to clear state file", "component", "state", "error", err)
	}
}

// AutoResume attempts to restore interrupted workflows from a saved state.
// Called once during bot startup after all initialization is complete.
func (b *BotService) AutoResume(ctx context.Context, chatID int64) {
	snap, err := b.LoadState()
	if err != nil {
		slog.Error("failed to load state for auto-resume", "component", "state", "error", err)
		return
	}
	if snap == nil {
		return // No saved state
	}

	slog.Info("auto-resuming from saved state", "component", "state", "state", snap.CurrentState, "account_key", accountKey(snap.StateAccountID))

	// Resume running tasks
	if len(snap.RunningTasks) > 0 {
		b.send(chatID, fmt.Sprintf("🔄 Auto-resuming %d interrupted booking task(s)...", len(snap.RunningTasks)))
		for _, task := range snap.RunningTasks {
			acc := b.config.GetAccount(task.AccountID)
			if acc == nil {
				slog.Warn("cannot resume task, account not found", "component", "state", "account_key", accountKey(task.AccountID))
				continue
			}
			if acc.SelectedCount() == 0 {
				slog.Warn("cannot resume task, no patients selected", "component", "state", "account_key", accountKey(task.AccountID))
				continue
			}
			// Never guess a target. Resuming a tomorrow-06:00 run against a
			// fabricated today-06:00 burned the session at the wrong date.
			// A run interrupted between 06:00 and 12:00 resumes and bursts now.
			if task.TargetTime.IsZero() || bookingClosed(task.TargetTime, time.Now()) {
				slog.Warn("cannot resume task, target time missing or passed", "component", "state",
					"account_key", accountKey(task.AccountID), "target", task.TargetTime)
				b.send(chatID, MsgResumeSkipped(acc.Name, task.TargetTime))
				continue
			}
			label := ""
			if len(snap.RunningTasks) > 1 {
				label = acc.Name
			}
			safego("resumeAccount", func() { b.runAccount(ctx, chatID, acc, task.TargetTime, label) })
		}
	}

	// If there were OTP waiters, notify user
	if len(snap.OTPWaiters) > 0 {
		var lines []string
		for _, accID := range snap.OTPWaiters {
			lines = append(lines, fmt.Sprintf("• <code>%s</code>", accID))
		}
		b.sendHTML(chatID, fmt.Sprintf(
			"⚠️ <b>Interrupted OTP Flows</b>\n\nThe following accounts were waiting for OTP when the bot stopped:\n\n%s\n\nIf booking is still needed, restart it with 📅 Book.",
			strings.Join(lines, "\n"),
		))
	}

	// If there were pending logins, notify user
	if len(snap.LoginStates) > 0 {
		var lines []string
		for accID, phase := range snap.LoginStates {
			lines = append(lines, fmt.Sprintf("• <code>%s</code> (phase: %s)", accID, phase))
		}
		b.sendHTML(chatID, fmt.Sprintf(
			"⚠️ <b>Interrupted Login Flows</b>\n\nThe following accounts were in login when the bot stopped:\n\n%s\n\nTap 🔐 Log in to re-authenticate.",
			strings.Join(lines, "\n"),
		))
	}

	// Clean up state file after resume attempt
	b.ClearStateFile()
}
