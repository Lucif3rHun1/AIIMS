package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"aiims-appointment/pkg/abdm"
	"aiims-appointment/pkg/config"
)

func convertPatients(configPatients []config.Patient) []abdm.Patient {
	abdmPatients := make([]abdm.Patient, len(configPatients))
	for i, p := range configPatients {
		abdmPatients[i] = abdm.Patient{
			OID:       p.OID,
			FLN:       p.FLN,
			HealthIDs: p.HealthIDs,
			ABHA:      p.ABHA,
		}
	}
	return abdmPatients
}

type accountRunner struct {
	accountID    string
	accountName  string
	patients     []abdm.Patient
	manager      *abdm.ABDMManager
	cancel       context.CancelFunc
	running      atomic.Bool
	successCount atomic.Int32
	failCount    atomic.Int32
}

func newAccountRunner(accountID, accountName string, selectedPatients []config.Patient) *accountRunner {
	return &accountRunner{
		accountID:   accountID,
		accountName: accountName,
		patients:    convertPatients(selectedPatients),
	}
}

func (r *accountRunner) start(ctx context.Context, cfg *config.Config, targetTime time.Time,
	sendMsg func(string), getOTP func(string) (string, error), onStatus abdm.StatusCallback) error {

	if !r.running.CompareAndSwap(false, true) {
		return fmt.Errorf("runner already running for account %s", r.accountName)
	}
	defer r.running.Store(false)

	taskCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	defer func() {
		r.cancel = nil
		cancel()
	}()

	if len(r.patients) == 0 {
		return fmt.Errorf("no selected patients for %s", r.accountName)
	}

	manager := abdm.NewABDMManager(cfg, taskCtx)
	r.manager = manager
	manager.SendMessage = sendMsg
	manager.GetOTP = getOTP
	manager.OnStatus = onStatus

	if err := manager.InitializeSystem(); err != nil {
		return fmt.Errorf("init failed for %s: %v", r.accountName, err)
	}

	slog.Info("validating patients", "component", "runner", "count", len(r.patients), "account", r.accountName)
	if err := manager.ValidatePatients(r.patients); err != nil {
		return fmt.Errorf("validation failed for %s: %v", r.accountName, err)
	}

	validated := manager.GetValidatedPool()
	slog.Info("patients validated", "component", "runner", "validated", len(validated), "total", len(r.patients), "account", r.accountName)

	return manager.ExecuteTaskWithRenewal(targetTime)
}

func (r *accountRunner) stop() {
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	if r.manager != nil {
		r.manager.Close()
		r.manager = nil
	}
}

type runnerRegistry struct {
	runners map[string]*accountRunner
	mu      sync.RWMutex
}

func newRunnerRegistry() *runnerRegistry {
	return &runnerRegistry{
		runners: make(map[string]*accountRunner),
	}
}

func (rr *runnerRegistry) add(r *accountRunner) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if existing, ok := rr.runners[r.accountID]; ok {
		slog.Info("stopping existing runner", "component", "runner", "account", existing.accountName)
		existing.stop()
	}
	rr.runners[r.accountID] = r
}

func (rr *runnerRegistry) remove(accountID string) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	delete(rr.runners, accountID)
}

func (rr *runnerRegistry) get(accountID string) *accountRunner {
	rr.mu.RLock()
	defer rr.mu.RUnlock()
	return rr.runners[accountID]
}

func (rr *runnerRegistry) all() []*accountRunner {
	rr.mu.RLock()
	defer rr.mu.RUnlock()
	result := make([]*accountRunner, 0, len(rr.runners))
	for _, r := range rr.runners {
		result = append(result, r)
	}
	return result
}

func (rr *runnerRegistry) anyRunning() bool {
	rr.mu.RLock()
	defer rr.mu.RUnlock()
	for _, r := range rr.runners {
		if r.running.Load() {
			return true
		}
	}
	return false
}

func (rr *runnerRegistry) stopAll() {
	rr.mu.RLock()
	runners := make([]*accountRunner, 0, len(rr.runners))
	for _, r := range rr.runners {
		runners = append(runners, r)
	}
	rr.mu.RUnlock()

	for _, r := range runners {
		slog.Info("stopping runner", "component", "runner", "account", r.accountName)
		r.stop()
	}

	rr.mu.Lock()
	for k := range rr.runners {
		delete(rr.runners, k)
	}
	rr.mu.Unlock()
}

func (rr *runnerRegistry) count() int {
	rr.mu.RLock()
	defer rr.mu.RUnlock()
	return len(rr.runners)
}
