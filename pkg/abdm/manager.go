package abdm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"aiims-appointment/pkg/config"
	"golang.org/x/time/rate"
)

const (
	BurstCount            = 10
	TargetInterval        = 100 * time.Millisecond
	maxValidationWorkers  = 5
	maxConcurrentPatients = 10
)

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

	tm := NewTokenManager(cfg, httpClient, masterToken)
	managerCtx, managerCancel := context.WithCancel(ctx)

	return &ABDMManager{
		config:            cfg,
		tokenManager:      tm,
		httpClient:        httpClient,
		originalTokens:    make(map[string]string),
		patientTokenCache: make(map[string]*Token),
		ctx:               managerCtx,
		cancel:            managerCancel,
		GetOTP:            func(hid string) (string, error) { return "", fmt.Errorf("OTP callback missing") },
		SendMessage:       func(msg string) { log.Println(msg) },
		OnStatus:          func(event string, details map[string]interface{}) {},
	}
}

func (am *ABDMManager) Close() {
	am.cancel()
	done := make(chan struct{})
	go func() {
		am.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Println("[manager] timeout waiting for goroutines to finish")
	}
}

func (am *ABDMManager) InitializeSystem() error {
	log.Println("Initializing system...")
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
	sem := make(chan struct{}, maxValidationWorkers)
	var mu sync.Mutex
	validated := 0

	for i, p := range patients {
		if p.PrimaryHealthID() == "" {
			continue
		}

		am.wg.Add(1)
		go func(p Patient, idx int) {
			defer am.wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			token, err := am.tokenManager.GetPatientToken(p.OID)
			if err != nil {
				errMsg := fmt.Sprintf("Token switch failed for %s: %v", p.FLN, err)
				log.Println(errMsg)
				am.OnStatus("validation_failed", map[string]interface{}{
					"patient": p.FLN, "error": errMsg, "index": idx,
				})
				return
			}

			if !am.checkHIPAccess(token) {
				if err := am.performOTPAuth(token, p.PrimaryHealthID()); err != nil {
					errMsg := fmt.Sprintf("OTP auth failed for %s: %v", p.FLN, err)
					log.Println(errMsg)
					am.OnStatus("validation_failed", map[string]interface{}{
						"patient": p.FLN, "error": errMsg, "index": idx,
					})
					return
				}
				if !am.checkHIPAccess(token) {
					am.OnStatus("validation_failed", map[string]interface{}{
						"patient": p.FLN, "error": "HIP access denied after OTP", "index": idx,
					})
					return
				}
			}

			mu.Lock()
			am.poolMutex.Lock()
			am.validatedPool = append(am.validatedPool, &ValidatedPatient{
				Patient:        p,
				ValidationDone: true,
			})
			validated++
			count := validated
			am.poolMutex.Unlock()
			mu.Unlock()

			am.OnStatus("validation_progress", map[string]interface{}{
				"patient": p.FLN, "validated": count, "total": len(patients),
			})
		}(p, i)
	}

	am.wg.Wait()
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
	_, status, _ := makeHTTPCall(am.ctx, am.httpClient, "GET", url, nil, token)
	return status == 200
}

func (am *ABDMManager) performOTPAuth(token *Token, healthID string) error {
	initPayload := map[string]string{"auth_method": "MOBILE_OTP", "health_id": healthID}
	body, status, err := makeHTTPCall(am.ctx, am.httpClient, "POST", "https://ndhm.eka.care/v1/auth/init", initPayload, token)
	if err != nil || status != 200 {
		return fmt.Errorf("OTP init failed")
	}

	var resp map[string]string
	json.Unmarshal(body, &resp)
	txnID := resp["txn_id"]

	otp, err := am.GetOTP(healthID)
	if err != nil {
		return err
	}

	verifyPayload := map[string]string{"otp": otp, "health_id": healthID, "txn_id": txnID}
	_, vStatus, vErr := makeHTTPCall(am.ctx, am.httpClient, "POST", "https://ndhm.eka.care/v1/auth/verify", verifyPayload, token)
	if vErr != nil || vStatus != 200 {
		return fmt.Errorf("OTP verify failed")
	}
	return nil
}

func (am *ABDMManager) ExecuteTask(targetTime time.Time) error {
	pool := am.GetValidatedPool()
	if len(pool) == 0 {
		return fmt.Errorf("no validated patients")
	}

	wait := time.Until(targetTime)
	if wait > 0 {
		am.SendMessage(fmt.Sprintf("Waiting %v until target time...", wait))
		am.OnStatus("waiting", map[string]interface{}{
			"wait_seconds": wait.Seconds(), "target": targetTime.Format("15:04:05"),
		})

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-am.ctx.Done():
			timer.Stop()
			return am.ctx.Err()
		}
	}

	am.OnStatus("execution_started", map[string]interface{}{
		"total_patients": len(pool), "target": targetTime.Format("02-01-2006 15:04:05"),
	})

	sem := make(chan struct{}, maxConcurrentPatients)

	for _, vp := range pool {
		am.wg.Add(1)
		go func(vp *ValidatedPatient) {
			defer am.wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			token, err := am.tokenManager.GetPatientToken(vp.Patient.OID)
			if err != nil {
				am.SendMessage(fmt.Sprintf("❌ Token error for %s: %v", vp.Patient.FLN, err))
				am.failCount.Add(1)
				am.OnStatus("patient_failed", map[string]interface{}{
					"patient": vp.Patient.FLN, "error": err.Error(),
				})
				return
			}
			am.performBurst(vp.Patient, token)
		}(vp)
	}

	am.wg.Wait()

	success := am.successCount.Load()
	fail := am.failCount.Load()
	am.OnStatus("execution_complete", map[string]interface{}{
		"success": success, "failed": fail, "total": len(pool),
	})

	return nil
}

func (am *ABDMManager) performBurst(p Patient, token *Token) {
	payload := map[string]interface{}{
		"hip_id":    am.config.HipID,
		"health_id": p.PrimaryHealthID(),
	}

	limiter := rate.NewLimiter(rate.Every(TargetInterval), BurstCount)
	var wg sync.WaitGroup
	var success atomic.Bool

	for i := 0; i < BurstCount; i++ {
		if success.Load() {
			break
		}

		wg.Add(1)
		go func(attempt int) {
			defer wg.Done()

			if err := limiter.Wait(am.ctx); err != nil {
				return
			}

			if success.Load() {
				return
			}

			resp, status, httpErr := makeHTTPCall(am.ctx, am.httpClient, "POST", "https://ndhm.eka.care/v2/hip/profile/share", payload, token)
			if httpErr != nil {
				am.OnStatus("burst_attempt_error", map[string]interface{}{
					"patient": p.FLN, "attempt": attempt, "error": httpErr.Error(),
				})
				return
			}

			if status == 200 {
				tokenNum := extractTokenNumber(resp)
				if tokenNum != "" {
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
				}
			}
		}(i)
	}

	wg.Wait()

	if !success.Load() {
		am.failCount.Add(1)
		am.SendMessage(fmt.Sprintf("❌ FAILED: %s", p.FLN))
		am.OnStatus("patient_failed", map[string]interface{}{
			"patient": p.FLN, "error": "all burst attempts exhausted",
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
