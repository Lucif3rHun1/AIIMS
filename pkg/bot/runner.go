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
	accountID   string
	accountName string
	patients    []abdm.Patient

	// target and startedAt are read by SaveState so a crash can resume this run
	// at its real slot instead of guessing.
	target    time.Time
	startedAt time.Time

	// mu guards manager and cancel: start() writes them from the runner
	// goroutine while stop() reads them from whichever goroutine cancels.
	mu      sync.Mutex
	manager *abdm.ABDMManager
	cancel  context.CancelFunc

	running      atomic.Bool
	successCount atomic.Int32
	failCount    atomic.Int32
}

func newAccountRunner(accountID, accountName string, selectedPatients []config.Patient, target time.Time) *accountRunner {
	return &accountRunner{
		accountID:   accountID,
		accountName: accountName,
		patients:    convertPatients(selectedPatients),
		target:      target,
		startedAt:   time.Now(),
	}
}

func (r *accountRunner) start(ctx context.Context, cfg *config.Config, targetTime time.Time,
	sendMsg func(string), getOTP func(string) (string, error), onStatus abdm.StatusCallback) error {

	if !r.running.CompareAndSwap(false, true) {
		return fmt.Errorf("runner already running for account %s", r.accountName)
	}
	defer r.running.Store(false)

	taskCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.cancel = nil
		r.mu.Unlock()
		cancel()
	}()

	if len(r.patients) == 0 {
		return fmt.Errorf("no selected patients for %s", r.accountName)
	}

	manager := abdm.NewABDMManager(cfg, taskCtx)
	r.mu.Lock()
	r.manager = manager
	r.mu.Unlock()
	manager.SendMessage = sendMsg
	manager.GetOTP = getOTP
	manager.OnStatus = onStatus

	if err := manager.InitializeSystem(); err != nil {
		return fmt.Errorf("init failed for %s: %v", r.accountName, err)
	}

	slog.Info("validating patients", "component", "runner", "count", len(r.patients), "account_key", accountKey(r.accountID))
	if err := manager.ValidatePatients(r.patients); err != nil {
		return fmt.Errorf("validation failed for %s: %v", r.accountName, err)
	}

	validated := manager.GetValidatedPool()
	slog.Info("patients validated", "component", "runner", "validated", len(validated), "total", len(r.patients), "account_key", accountKey(r.accountID))

	return manager.ExecuteTaskWithRenewal(targetTime)
}

func (r *accountRunner) stop() {
	r.mu.Lock()
	cancel, manager := r.cancel, r.manager
	r.cancel, r.manager = nil, nil
	r.mu.Unlock()

	// Close() blocks for up to 10s; never call it holding a lock others need.
	if cancel != nil {
		cancel()
	}
	if manager != nil {
		manager.Close()
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
	existing := rr.runners[r.accountID]
	rr.runners[r.accountID] = r
	rr.mu.Unlock()

	// stop() blocks up to 10s inside manager.Close(). Holding rr.mu across it
	// froze every other registry reader — status, stop-all, the whole menu.
	if existing != nil {
		slog.Info("stopping existing runner", "component", "runner", "account_key", accountKey(existing.accountID))
		existing.stop()
	}
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
	rr.mu.Lock()
	runners := make([]*accountRunner, 0, len(rr.runners))
	for _, r := range rr.runners {
		runners = append(runners, r)
	}
	rr.runners = make(map[string]*accountRunner)
	rr.mu.Unlock()

	// Each stop() can take 10s. Serially that overruns main's 15s force-exit
	// as soon as there are two accounts, and the process dies with code 1.
	var wg sync.WaitGroup
	for _, r := range runners {
		wg.Add(1)
		go func(r *accountRunner) {
			defer wg.Done()
			r.stop()
		}(r)
	}
	wg.Wait()
}

func (rr *runnerRegistry) count() int {
	rr.mu.RLock()
	defer rr.mu.RUnlock()
	return len(rr.runners)
}
