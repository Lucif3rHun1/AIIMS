package abdm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"aiims-appointment/pkg/config"
)

const (
	maxValidationWorkers  = 5
	maxConcurrentPatients = 10

	SpamDuration          = 60 * time.Second
	SpamInterval          = 1 * time.Second
	ConfirmationThreshold = 5
	MaxRenewalDuration    = 6 * time.Hour
	// ponytail: MinRenewalDuration no longer gates the renewal loop (it stops as soon as
	// every patient is confirmed). Kept only because manager_test.go asserts it; delete both.
	MinRenewalDuration = 4 * time.Hour
)

type ConfirmationTracker struct {
	mu            sync.Mutex
	lastToken     string
	confirmCount  int
	threshold     int
	confirmed     bool
	totalAttempts int
	tokenNumbers  []string
}

func NewConfirmationTracker(threshold int) *ConfirmationTracker {
	return &ConfirmationTracker{
		threshold:    threshold,
		tokenNumbers: make([]string, 0),
	}
}

func (ct *ConfirmationTracker) Record(tokenNumber string) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	ct.totalAttempts++

	if tokenNumber == "" {
		ct.confirmCount = 0
		ct.lastToken = ""
		return false
	}

	ct.tokenNumbers = append(ct.tokenNumbers, tokenNumber)

	if tokenNumber == ct.lastToken {
		ct.confirmCount++
	} else {
		ct.lastToken = tokenNumber
		ct.confirmCount = 1
	}

	if ct.confirmCount >= ct.threshold {
		ct.confirmed = true
		return true
	}
	return false
}

func (ct *ConfirmationTracker) IsConfirmed() bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.confirmed
}

func (ct *ConfirmationTracker) GetStats() (lastToken string, confirmCount int, totalAttempts int, confirmed bool) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.lastToken, ct.confirmCount, ct.totalAttempts, ct.confirmed
}

type StatusCallback func(event string, details map[string]interface{})

type ABDMManager struct {
	config         *config.Config
	tokenManager   TokenManager
	httpClient     HTTPClient
	validatedPool  []*ValidatedPatient
	poolMutex      sync.RWMutex
	originalTokens map[string]string
	tokenMutex     sync.RWMutex
	successCount   atomic.Int32
	failCount      atomic.Int32
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup

	patientTokenCache map[string]*Token
	cacheMutex        sync.RWMutex

	GetOTP      func(healthID string) (string, error)
	SendMessage func(msg string)
	OnStatus    StatusCallback
}

func NewABDMManager(cfg *config.Config, ctx context.Context) *ABDMManager {
	httpClient := NewHTTPClient()

	deviceID, _ := GenerateSecureUUID()
	masterToken := &Token{
		Auth:      cfg.AuthToken,
		Sess:      cfg.AuthToken,
		Refresh:   cfg.RefreshToken,
		DeviceID:  deviceID,
		ExpiresAt: ParseJWTExpiry(cfg.AuthToken),
	}

	managerCtx, managerCancel := context.WithCancel(ctx)
	tm := NewTokenManager(cfg, httpClient, masterToken, managerCtx)

	return &ABDMManager{
		config:            cfg,
		tokenManager:      tm,
		httpClient:        httpClient,
		originalTokens:    make(map[string]string),
		patientTokenCache: make(map[string]*Token),
		ctx:               managerCtx,
		cancel:            managerCancel,
		GetOTP:            func(hid string) (string, error) { return "", fmt.Errorf("OTP callback missing") },
		SendMessage:       func(msg string) { slog.Info("status message", "component", "manager", "message", msg) },
		OnStatus:          func(event string, details map[string]interface{}) {},
	}
}

// Close cancels the manager context. am.wg belongs to fireBurst's own round; waiting on
// it here too trips "WaitGroup misuse" when a renewal round Adds while Close is waiting.
// ponytail: a burst goroutine parked in handlePersistentResponse's time.Sleep(retryAfter)
// outlives Close. Make that sleep ctx-aware if shutdown latency ever matters.
func (am *ABDMManager) Close() { am.cancel() }

func (am *ABDMManager) InitializeSystem() error {
	slog.Info("initializing system", "component", "manager")
	if err := am.tokenManager.RefreshMasterToken(); err != nil {
		return fmt.Errorf("initial token refresh failed: %v", err)
	}
	return nil
}

func (am *ABDMManager) FetchPatients() ([]Patient, error) {
	body, status, err := makeHTTPCall(am.ctx, am.httpClient, "GET", "https://aortago.eka.care/profiles/v1/patient", nil, am.tokenManager.GetMasterToken())
	if err != nil || status != 200 {
		return nil, fmt.Errorf("fetch failed: %v (status %d)", err, status)
	}

	var patients []Patient
	if err := json.Unmarshal(body, &patients); err != nil {
		return nil, err
	}
	return patients, nil
}

func (am *ABDMManager) ValidatePatients(patients []Patient) error {
	am.poolMutex.Lock()
	am.validatedPool = am.validatedPool[:0]
	am.poolMutex.Unlock()

	validated := 0

	for i, p := range patients {
		select {
		case <-am.ctx.Done():
			return am.ctx.Err()
		default:
		}

		if p.PrimaryHealthID() == "" {
			am.OnStatus("validation_failed", map[string]interface{}{
				"patient": p.FLN, "error": "no primary health ID", "index": i,
			})
			continue
		}

		token, err := am.tokenManager.GetPatientToken(p.OID)
		if err != nil {
			errMsg := fmt.Sprintf("Token switch failed for %s: %v", p.FLN, err)
			slog.Error("token switch failed", "component", "manager", "message", errMsg)
			am.OnStatus("validation_failed", map[string]interface{}{
				"patient": p.FLN, "error": errMsg, "index": i,
			})
			continue
		}

		if !am.checkHIPAccess(token) {
			if err := am.performOTPAuth(token, p.PrimaryHealthID()); err != nil {
				errMsg := fmt.Sprintf("OTP auth failed for %s: %v", p.FLN, err)
				slog.Error("OTP authentication failed", "component", "manager", "message", errMsg)
				am.OnStatus("validation_failed", map[string]interface{}{
					"patient": p.FLN, "error": errMsg, "index": i,
				})
				continue
			}
			if !am.checkHIPAccess(token) {
				am.OnStatus("validation_failed", map[string]interface{}{
					"patient": p.FLN, "error": "HIP access denied after OTP", "index": i,
				})
				continue
			}
		}

		am.poolMutex.Lock()
		am.validatedPool = append(am.validatedPool, &ValidatedPatient{
			Patient:        p,
			ValidationDone: true,
		})
		validated++
		count := validated
		am.poolMutex.Unlock()

		am.OnStatus("validation_progress", map[string]interface{}{
			"patient": p.FLN, "validated": count, "total": len(patients),
		})
	}

	if validated == 0 {
		return fmt.Errorf("all %d patients failed validation", len(patients))
	}
	return nil
}

func (am *ABDMManager) GetValidatedPool() []*ValidatedPatient {
	am.poolMutex.RLock()
	defer am.poolMutex.RUnlock()
	pool := make([]*ValidatedPatient, len(am.validatedPool))
	copy(pool, am.validatedPool)
	return pool
}

func (am *ABDMManager) checkHIPAccess(token *Token) bool {
	url := fmt.Sprintf("https://ndhm.eka.care/v1/hip/providers/search?hip_id=%s", am.config.HipID)
	_, status, err := makeHTTPCall(am.ctx, am.httpClient, "GET", url, nil, token)
	if status != 200 {
		slog.Error("HIP access check failed", "component", "manager", "status", status, "error", err)
	}
	return status == 200
}

func (am *ABDMManager) performOTPAuth(token *Token, healthID string) error {
	initPayload := map[string]string{"auth_method": "MOBILE_OTP", "health_id": healthID}
	body, status, err := makeHTTPCall(am.ctx, am.httpClient, "POST", "https://ndhm.eka.care/v1/auth/init", initPayload, token)
	if err != nil || status != 200 {
		slog.Error("OTP init failed", "component", "manager", "status", status, "error", err)
		return fmt.Errorf("OTP init failed (status %d)", status)
	}

	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("OTP init response parse failed: %v", err)
	}
	txnID := resp["txn_id"]
	if txnID == "" {
		return fmt.Errorf("OTP init response missing txn_id")
	}

	otp, err := am.GetOTP(healthID)
	if err != nil {
		return fmt.Errorf("OTP input failed: %v", err)
	}

	verifyPayload := map[string]string{"otp": otp, "health_id": healthID, "txn_id": txnID}
	_, vStatus, vErr := makeHTTPCall(am.ctx, am.httpClient, "POST", "https://ndhm.eka.care/v1/auth/verify", verifyPayload, token)
	if vErr != nil || vStatus != 200 {
		slog.Error("OTP verify failed", "component", "manager", "status", vStatus, "error", vErr)
		return fmt.Errorf("OTP verify failed (status %d)", vStatus)
	}

	return nil
}

func (am *ABDMManager) ExecuteTask(targetTime time.Time) error {
	am.successCount.Store(0)
	am.failCount.Store(0)

	pool := am.GetValidatedPool()
	if len(pool) == 0 {
		return fmt.Errorf("no validated patients")
	}

	const preWarmOffset = 30 * time.Second

	wait := time.Until(targetTime)
	if wait > 0 {
		am.SendMessage(fmt.Sprintf("⏳ Waiting %v until target time...", wait.Round(time.Minute)))
		am.OnStatus("waiting", map[string]interface{}{
			"wait_seconds": wait.Seconds(), "target": targetTime.Format("15:04:05"),
		})

		preWarmTime := targetTime.Add(-preWarmOffset)
		preWarmWait := time.Until(preWarmTime)

		if preWarmWait > 0 {
			preWarmTimer := time.NewTimer(preWarmWait)
			select {
			case <-preWarmTimer.C:
			case <-am.ctx.Done():
				preWarmTimer.Stop()
				return am.ctx.Err()
			}
		}

		// Phase 2: Pre-warm — refresh master token + pre-switch all patient tokens
		am.OnStatus("prewarm_started", map[string]interface{}{
			"total_patients": len(pool),
			"t_minus":        preWarmOffset.String(),
		})

		slog.Info("pre-warm: refreshing master token", "component", "manager")
		if err := am.tokenManager.RefreshMasterToken(); err != nil {
			return fmt.Errorf("pre-warm master token refresh failed: %v", err)
		}
		slog.Info("pre-warm: master token refreshed", "component", "manager")

		am.OnStatus("prewarm_token_refreshed", map[string]interface{}{
			"total_patients": len(pool),
		})

		cachedTokens := make(map[string]*Token, len(pool))
		preWarmSuccess := 0
		preWarmFailed := 0

		for _, vp := range pool {
			select {
			case <-am.ctx.Done():
				return am.ctx.Err()
			default:
			}

			token, err := am.tokenManager.GetPatientToken(vp.Patient.OID)
			if err != nil {
				slog.Error("pre-warm token switch failed", "component", "manager", "patient", vp.Patient.FLN, "error", err)
				preWarmFailed++
				continue
			}
			cachedTokens[vp.Patient.OID] = token
			preWarmSuccess++

			am.OnStatus("prewarm_progress", map[string]interface{}{
				"patient": vp.Patient.FLN,
				"ready":   preWarmSuccess,
				"total":   len(pool),
			})
		}

		am.OnStatus("prewarm_complete", map[string]interface{}{
			"ready": preWarmSuccess, "failed": preWarmFailed, "total": len(pool),
		})

		if late := time.Since(targetTime); late > 0 {
			slog.Warn("pre-warm overran target time", "component", "manager", "late", late, "ready", preWarmSuccess)
			am.SendMessage(fmt.Sprintf("⚠️ Pre-warm overran T-0 by %v — burst is late", late.Round(time.Second)))
			am.OnStatus("prewarm_overran", map[string]interface{}{
				"late": late.String(), "ready": preWarmSuccess,
			})
		} else {
			am.SendMessage(fmt.Sprintf("🔥 Pre-warmed %d/%d patients. Waiting for T-0...", preWarmSuccess, len(pool)))
		}

		// Phase 3: Wait remaining time until exact target
		remainingWait := time.Until(targetTime)
		if remainingWait > 0 {
			remainingTimer := time.NewTimer(remainingWait)
			select {
			case <-remainingTimer.C:
			case <-am.ctx.Done():
				remainingTimer.Stop()
				return am.ctx.Err()
			}
		}

		// Phase 4: BURST with pre-cached tokens — no HTTP overhead
		am.OnStatus("execution_started", map[string]interface{}{
			"total_patients": len(pool), "target": targetTime.Format("02-01-2006 15:04:05"),
		})

		am.fireBurst(pool, cachedTokens)
	} else {
		// Target already passed — burst immediately with fresh tokens
		slog.Warn("target time passed, bursting immediately", "component", "manager")
		if err := am.tokenManager.RefreshMasterToken(); err != nil {
			return fmt.Errorf("immediate burst token refresh failed: %v", err)
		}

		am.OnStatus("execution_started", map[string]interface{}{
			"total_patients": len(pool), "target": targetTime.Format("02-01-2006 15:04:05"),
		})

		cachedTokens := make(map[string]*Token, len(pool))
		for _, vp := range pool {
			token, err := am.tokenManager.GetPatientToken(vp.Patient.OID)
			if err != nil {
				slog.Error("token switch failed", "component", "manager", "patient", vp.Patient.FLN, "error", err)
				continue
			}
			cachedTokens[vp.Patient.OID] = token
		}

		am.fireBurst(pool, cachedTokens)
	}

	success := am.successCount.Load()
	fail := am.failCount.Load()
	am.OnStatus("execution_complete", map[string]interface{}{
		"success": success, "failed": fail, "total": len(pool),
	})

	return nil
}

func (am *ABDMManager) ExecuteTaskWithRenewal(targetTime time.Time) error {
	startTime := time.Now()

	if err := am.ExecuteTask(targetTime); err != nil {
		return fmt.Errorf("initial execution failed: %v", err)
	}

	am.OnStatus("renewal_loop_entered", map[string]interface{}{
		"max_duration": MaxRenewalDuration.String(),
	})

	for {
		elapsed := time.Since(startTime)
		if elapsed >= MaxRenewalDuration {
			am.SendMessage(fmt.Sprintf("⏹ Max renewal duration (%v) reached, stopping", MaxRenewalDuration))
			am.OnStatus("renewal_max_reached", map[string]interface{}{
				"elapsed": elapsed.String(),
			})
			return nil
		}

		pool := am.GetValidatedPool()
		if int(am.successCount.Load()) >= len(pool) {
			am.SendMessage(fmt.Sprintf("✅ All %d patients confirmed after %v, stopping", am.successCount.Load(), elapsed.Round(time.Minute)))
			am.OnStatus("renewal_all_confirmed", map[string]interface{}{
				"elapsed": elapsed.String(),
				"success": am.successCount.Load(),
			})
			return nil
		}

		now := time.Now()
		nextHour := time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+1, 0, 0, 0, now.Location())
		renewalTime := nextHour.Add(-1 * time.Minute)

		if renewalTime.Before(now.Add(10 * time.Second)) {
			renewalTime = time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+2, 0, 0, 0, now.Location()).Add(-1 * time.Minute)
		}

		waitDuration := time.Until(renewalTime)
		am.OnStatus("renewal_scheduled", map[string]interface{}{
			"renewal_at": renewalTime.Format("15:04:05"),
			"wait":       waitDuration.Round(time.Second).String(),
			"elapsed":    elapsed.Round(time.Second).String(),
		})
		am.SendMessage(fmt.Sprintf("🔄 Next renewal at %v (wait %v)", renewalTime.Format("15:04:05"), waitDuration.Round(time.Minute)))

		waitTimer := time.NewTimer(waitDuration)
		select {
		case <-am.ctx.Done():
			waitTimer.Stop()
			return am.ctx.Err()
		case <-waitTimer.C:
		}

		am.OnStatus("renewal_started", map[string]interface{}{
			"elapsed": time.Since(startTime).Round(time.Second).String(),
		})
		am.SendMessage("🔄 Renewal: refreshing master token...")

		if err := am.tokenManager.RefreshMasterToken(); err != nil {
			slog.Error("renewal master token refresh failed", "component", "manager", "error", err)
			am.OnStatus("renewal_token_failed", map[string]interface{}{
				"error": err.Error(),
			})
			continue
		}

		pool = am.GetValidatedPool()
		cachedTokens := make(map[string]*Token, len(pool))
		for _, vp := range pool {
			token, err := am.tokenManager.GetPatientToken(vp.Patient.OID)
			if err != nil {
				slog.Error("renewal token switch failed", "component", "manager", "patient", vp.Patient.FLN, "error", err)
				continue
			}
			cachedTokens[vp.Patient.OID] = token
		}

		am.successCount.Store(0)
		am.failCount.Store(0)

		am.OnStatus("renewal_burst_started", map[string]interface{}{
			"total_patients": len(pool),
		})

		am.fireBurst(pool, cachedTokens)

		success := am.successCount.Load()
		fail := am.failCount.Load()
		am.OnStatus("renewal_complete", map[string]interface{}{
			"success": success, "failed": fail, "total": len(pool),
			"elapsed": time.Since(startTime).Round(time.Second).String(),
		})
		am.SendMessage(fmt.Sprintf("🔄 Renewal cycle done: %d confirmed, %d failed", success, fail))
	}
}

func (am *ABDMManager) persistentSpam(p Patient, token *Token, tracker *ConfirmationTracker) (string, error) {
	payload := map[string]interface{}{
		"hip_id":    am.config.HipID,
		"hip_code":  am.config.HipID,
		"health_id": p.PrimaryHealthID(),
		"location":  map[string]interface{}{},
	}

	deadline := time.After(SpamDuration)
	ticker := time.NewTicker(SpamInterval)
	defer ticker.Stop()

	// Fire first request immediately
	resp, status, retryAfter, httpErr := makeHTTPCallWithRetryAfter(am.ctx, am.httpClient, "POST", "https://ndhm.eka.care/v2/hip/profile/share", payload, token)
	if httpErr != nil {
		slog.Warn("persistentSpam HTTP error", "component", "manager", "patient", p.FLN, "error", httpErr)
	} else {
		tokenNum := am.handlePersistentResponse(status, retryAfter, resp, p, tracker)
		if tracker.IsConfirmed() {
			return tokenNum, nil
		}
		if status == 401 {
			return "", fmt.Errorf("unauthorized for %s", p.FLN)
		}
	}

	for {
		select {
		case <-am.ctx.Done():
			return "", am.ctx.Err()
		case <-deadline:
			lastToken, confirmCount, totalAttempts, confirmed := tracker.GetStats()
			slog.Info("persistentSpam deadline reached", "component", "manager",
				"confirmed", confirmed, "confirmCount", confirmCount, "totalAttempts", totalAttempts)
			if confirmed {
				return lastToken, nil
			}
			return "", fmt.Errorf("spam duration expired for %s (confirmed=%v, attempts=%d)", p.FLN, confirmed, totalAttempts)
		case <-ticker.C:
			resp, status, retryAfter, httpErr := makeHTTPCallWithRetryAfter(am.ctx, am.httpClient, "POST", "https://ndhm.eka.care/v2/hip/profile/share", payload, token)
			if httpErr != nil {
				slog.Warn("persistentSpam HTTP error", "component", "manager", "patient", p.FLN, "error", httpErr)
				continue
			}

			tokenNum := am.handlePersistentResponse(status, retryAfter, resp, p, tracker)
			if tracker.IsConfirmed() {
				return tokenNum, nil
			}
			if status == 401 {
				return "", fmt.Errorf("unauthorized for %s", p.FLN)
			}
		}
	}
}

func (am *ABDMManager) handlePersistentResponse(status int, retryAfter time.Duration, resp []byte, p Patient, tracker *ConfirmationTracker) string {
	if status == 200 {
		tokenNum := extractTokenNumber(resp)
		if tokenNum == "" {
			slog.Warn("persistentSpam 200 but no token_number", "component", "manager")
			am.OnStatus("persistent_no_token", map[string]interface{}{
				"patient": p.FLN, "response": string(resp),
			})
			return ""
		}

		tracker.Record(tokenNum)
		slog.Info("appointment_confirmed", "event", "appointment_confirmed")
		return tokenNum
	}

	transientStatuses := map[int]bool{429: true, 503: true, 502: true}
	if transientStatuses[status] {
		if status == 429 && retryAfter > 0 {
			time.Sleep(retryAfter)
		}
		slog.Warn("persistentSpam transient error", "component", "manager", "status", status)
		am.OnStatus("persistent_transient", map[string]interface{}{
			"patient": p.FLN, "status": status,
		})
		return ""
	}

	slog.Warn("persistentSpam non-200 response", "component", "manager", "status", status)
	am.OnStatus("persistent_failed_status", map[string]interface{}{
		"patient": p.FLN, "status": status, "response": string(resp),
	})
	return ""
}

func (am *ABDMManager) fireBurst(pool []*ValidatedPatient, cachedTokens map[string]*Token) {
	sem := make(chan struct{}, maxConcurrentPatients)

	for _, vp := range pool {
		token, ok := cachedTokens[vp.Patient.OID]
		if !ok {
			am.SendMessage(fmt.Sprintf("⚠️ No pre-warmed token for %s, skipping", vp.Patient.FLN))
			am.failCount.Add(1)
			am.OnStatus("patient_failed", map[string]interface{}{
				"patient": vp.Patient.FLN, "error": "no pre-warmed token",
			})
			continue
		}

		am.wg.Add(1)
		go func(vp *ValidatedPatient, token *Token) {
			defer am.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("fireBurst goroutine panicked, recovered", "component", "manager", "patient", vp.Patient.FLN, "panic", r)
					am.failCount.Add(1)
					am.OnStatus("patient_panic", map[string]interface{}{
						"patient": vp.Patient.FLN, "panic": fmt.Sprintf("%v", r),
					})
				}
			}()

			sem <- struct{}{}
			defer func() { <-sem }()

			tracker := NewConfirmationTracker(ConfirmationThreshold)
			tokenNum, err := am.persistentSpam(vp.Patient, token, tracker)
			if err != nil {
				am.failCount.Add(1)
				am.SendMessage(fmt.Sprintf("❌ FAILED: %s - %v", vp.Patient.FLN, err))
				am.OnStatus("patient_failed", map[string]interface{}{
					"patient": vp.Patient.FLN, "error": err.Error(),
				})
			} else {
				am.successCount.Add(1)
				am.SendMessage(fmt.Sprintf("✅ CONFIRMED: %s - Token: %s", vp.Patient.FLN, tokenNum))
				am.OnStatus("appointment_confirmed", map[string]interface{}{
					"patient":      vp.Patient.FLN,
					"token_number": tokenNum,
					"health_id":    vp.Patient.PrimaryHealthID(),
					"hip_name":     "AIIMS Raipur",
				})
			}
		}(vp, token)
	}

	am.wg.Wait()
}

func (am *ABDMManager) handleBurstResponse(status int, resp []byte, p Patient, attempt int, payload map[string]interface{}, token *Token, success *atomic.Bool) {
	if status == 200 {
		tokenNum := extractTokenNumber(resp)
		if tokenNum == "" {
			slog.Warn("burst success but no token_number", "component", "manager", "attempt", attempt, "patient", p.FLN)
			am.OnStatus("burst_attempt_no_token", map[string]interface{}{
				"patient": p.FLN, "attempt": attempt, "response": string(resp),
			})
			return
		}
		if success.CompareAndSwap(false, true) {
			am.successCount.Add(1)
			am.SendMessage(fmt.Sprintf("✅ SUCCESS: %s - Token: %s", p.FLN, tokenNum))
			am.OnStatus("appointment_confirmed", map[string]interface{}{
				"patient":      p.FLN,
				"token_number": tokenNum,
				"health_id":    p.PrimaryHealthID(),
				"hip_name":     "AIIMS Raipur",
				"attempt":      attempt,
			})
		}
		return
	}

	transientStatuses := map[int]bool{429: true, 503: true, 502: true}
	if transientStatuses[status] {
		am.retryWithBackoff(status, resp, p, attempt, payload, token, success)
		return
	}

	if status == 401 {
		slog.Error("burst unauthorized", "component", "manager", "attempt", attempt, "patient", p.FLN)
		am.OnStatus("burst_attempt_unauthorized", map[string]interface{}{
			"patient": p.FLN, "attempt": attempt, "response": string(resp),
		})
		return
	}

	slog.Warn("burst attempt status", "component", "manager", "attempt", attempt, "patient", p.FLN, "status", status)
	am.OnStatus("burst_attempt_failed", map[string]interface{}{
		"patient": p.FLN, "attempt": attempt, "status": status, "response": string(resp),
	})
}

func (am *ABDMManager) retryWithBackoff(status int, resp []byte, p Patient, attempt int, payload map[string]interface{}, token *Token, success *atomic.Bool) {
	backoff := time.Duration(math.Pow(2, float64(attempt)))*time.Second + time.Duration(rand.Intn(500))*time.Millisecond
	slog.Warn("burst retrying", "component", "manager", "attempt", attempt, "patient", p.FLN, "status", status, "retry_after", backoff)

	select {
	case <-time.After(backoff):
	case <-am.ctx.Done():
		return
	}

	if success.Load() {
		return
	}

	resp2, status2, httpErr2 := makeHTTPCall(am.ctx, am.httpClient, "POST", "https://ndhm.eka.care/v2/hip/profile/share", payload, token)
	if httpErr2 != nil {
		am.OnStatus("burst_retry_error", map[string]interface{}{
			"patient": p.FLN, "attempt": attempt, "error": httpErr2.Error(),
		})
		return
	}

	if status2 != 200 {
		slog.Warn("burst retry status", "component", "manager", "retry", attempt, "patient", p.FLN, "status", status2)
		am.OnStatus("burst_retry_failed", map[string]interface{}{
			"patient": p.FLN, "attempt": attempt, "status": status2, "response": string(resp2),
		})
		return
	}

	tokenNum := extractTokenNumber(resp2)
	if tokenNum == "" {
		slog.Warn("burst retry success but no token_number", "component", "manager", "retry", attempt, "patient", p.FLN)
		return
	}

	if success.CompareAndSwap(false, true) {
		am.successCount.Add(1)
		am.SendMessage(fmt.Sprintf("✅ SUCCESS (retry): %s - Token: %s", p.FLN, tokenNum))
		am.OnStatus("appointment_confirmed", map[string]interface{}{
			"patient":      p.FLN,
			"token_number": tokenNum,
			"health_id":    p.PrimaryHealthID(),
			"hip_name":     "AIIMS Raipur",
			"attempt":      attempt,
		})
	}
}

func extractTokenNumber(jsonData []byte) string {
	var m map[string]interface{}
	json.Unmarshal(jsonData, &m)
	if t, ok := m["token_number"].(string); ok {
		return t
	}
	if d, ok := m["data"].(map[string]interface{}); ok {
		if t, ok := d["token_number"].(string); ok {
			return t
		}
	}
	return ""
}
