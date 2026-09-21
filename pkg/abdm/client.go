package abdm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiims-appointment/pkg/config"
)

const (
	MaxRetryAttempts = 3
	RetryDelay       = 2 * time.Second
	RequestTimeout   = 10 * time.Second
	MaxResponseSize  = 10 * 1024 * 1024
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type TokenManager interface {
	GetMasterToken() *Token
	RefreshMasterToken() error
	GetPatientToken(patientOID string) (*Token, error)
	UpdateMasterToken(sess, refresh string)
}

type defaultTokenManager struct {
	masterToken *Token
	httpClient  HTTPClient
	config      *config.Config
	mutex       sync.RWMutex
	ctx         context.Context
}

func NewTokenManager(cfg *config.Config, client HTTPClient, initialToken *Token, ctx context.Context) TokenManager {
	return &defaultTokenManager{
		masterToken: initialToken,
		httpClient:  client,
		config:      cfg,
		ctx:         ctx,
	}
}

func (tm *defaultTokenManager) GetMasterToken() *Token {
	tm.mutex.RLock()
	defer tm.mutex.RUnlock()
	return tm.masterToken
}

func (tm *defaultTokenManager) RefreshMasterToken() error {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	payload := map[string]string{
		"sess":    tm.masterToken.Auth,
		"refresh": tm.masterToken.Refresh,
	}

	var lastErr error
	for attempt := 1; attempt <= MaxRetryAttempts; attempt++ {
		slog.Warn("token refresh attempt", "component", "token", "attempt", attempt, "max_attempts", MaxRetryAttempts)
		body, status, err := makeHTTPCall(tm.ctx, tm.httpClient, "POST", "https://aortago.eka.care/phr/v3/auth/refresh", payload, tm.masterToken)
		slog.Debug("token refresh response", "component", "token", "status", status, "error", err)
		if err == nil && status == 200 {
			var respObj map[string]interface{}
			if err := json.Unmarshal(body, &respObj); err != nil {
				lastErr = fmt.Errorf("failed to parse refresh response: %v", err)
				continue
			}

			sess, sessOk := respObj["sess"].(string)
			refresh, refreshOk := respObj["refresh"].(string)

			if !sessOk || !refreshOk || sess == "" || refresh == "" {
				lastErr = fmt.Errorf("invalid refresh response format")
				continue
			}

			deviceID, _ := GenerateSecureUUID()

			tm.masterToken = &Token{
				Auth:      sess,
				Sess:      sess,
				Refresh:   refresh,
				DeviceID:  deviceID,
				ExpiresAt: ParseJWTExpiry(sess),
			}

			tm.config.UpdateTokens(sess, refresh)
			slog.Info("tokens refreshed and persisted", "component", "token", "expires", ParseJWTExpiry(sess).Format("15:04:05"))

			return nil
		}

		lastErr = fmt.Errorf("status %d, err: %v", status, err)
		time.Sleep(RetryDelay)
	}
	return fmt.Errorf("refresh failed: %v", lastErr)
}

func (tm *defaultTokenManager) GetPatientToken(patientOID string) (*Token, error) {
	masterToken := tm.GetMasterToken()
	if masterToken.IsExpired() {
		if err := tm.RefreshMasterToken(); err != nil {
			return nil, err
		}
		masterToken = tm.GetMasterToken()
	}

	payload := map[string]string{
		"oid":     patientOID,
		"refresh": masterToken.Refresh,
	}

	body, status, err := makeHTTPCall(tm.ctx, tm.httpClient, "POST", "https://aortago.eka.care/v3/auth/switch", payload, masterToken)
	if err != nil || status != 200 {
		return nil, fmt.Errorf("switch failed: %v (status %d)", err, status)
	}

	var switchResponse map[string]string
	if err := json.Unmarshal(body, &switchResponse); err != nil {
		return nil, err
	}

	sess := switchResponse["sess"]
	refresh := switchResponse["refresh"]

	deviceID, _ := GenerateSecureUUID()

	return &Token{
		Auth:      sess,
		Sess:      sess,
		Refresh:   refresh,
		DeviceID:  deviceID,
		ExpiresAt: ParseJWTExpiry(sess),
	}, nil
}

// Helper to create HTTP Client
func NewHTTPClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout: RequestTimeout,
		Jar:     jar,
		Transport: &http.Transport{
			TLSClientConfig:    &tls.Config{InsecureSkipVerify: false},
			ForceAttemptHTTP2:  true,
			DisableCompression: true,
		},
	}
}

func makeHTTPCall(ctx context.Context, client HTTPClient, method, url string, payload interface{}, token *Token) ([]byte, int, error) {
	body, status, _, err := makeHTTPCallWithRetryAfter(ctx, client, method, url, payload, token)
	return body, status, err
}

func makeHTTPCallWithRetryAfter(ctx context.Context, client HTTPClient, method, url string, payload interface{}, token *Token) ([]byte, int, time.Duration, error) {
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}

	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, err
	}

	headers := map[string]string{
		"flavour":      "ios",
		"version":      "3.1.3",
		"Content-Type": "application/json",
		"User-Agent":   "EkaCare/3.3.1 (com.orbi.eka.care; build:11; iOS 26.4.2) Alamofire/5.11.1",
		"client-id":    "patient-app-ios",
		"Origin":       "file://",
	}

	if token != nil {
		headers["auth"] = token.Auth
		headers["Cookie"] = fmt.Sprintf("sess=%s; refresh=%s", token.Sess, token.Refresh)
		headers["device-id"] = token.DeviceID
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()

	retryAfter := time.Duration(0)
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); resp.StatusCode == 429 && err == nil && seconds > 0 {
		if seconds > 60 {
			seconds = 60
		}
		retryAfter = time.Duration(seconds) * time.Second
	}

	respBody, err := readLimitedResponse(resp, MaxResponseSize)
	return respBody, resp.StatusCode, retryAfter, err
}

func readLimitedResponse(resp *http.Response, maxSize int64) ([]byte, error) {
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	limitedReader := io.LimitReader(reader, maxSize)
	return io.ReadAll(limitedReader)
}

func GenerateSecureUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func ParseJWTExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]interface{}
	json.Unmarshal(payload, &claims)
	if exp, ok := claims["exp"].(float64); ok {
		return time.Unix(int64(exp), 0)
	}
	return time.Time{}
}

func (tm *defaultTokenManager) UpdateMasterToken(sess, refresh string) {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()
	deviceID, _ := GenerateSecureUUID()
	tm.masterToken = &Token{
		Auth:      sess,
		Sess:      sess,
		Refresh:   refresh,
		DeviceID:  deviceID,
		ExpiresAt: ParseJWTExpiry(sess),
	}
}

type ABDMLogin struct {
	httpClient HTTPClient
}

func NewABDMLogin() *ABDMLogin {
	return &ABDMLogin{httpClient: NewHTTPClient()}
}

func abdmHeaders() map[string]string {
	return map[string]string{
		"Content-Type":       "application/json",
		"client-id":          "patient-app-ios",
		"x-abha-sdk-version": "1.0.0",
		"User-Agent":         "Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148",
		"Origin":             "file://",
		"Accept":             "*/*",
		"Accept-Language":    "en-US,en;q=0.9",
	}
}

func (al *ABDMLogin) InitLogin(ctx context.Context, phone string) (*LoginInitResponse, error) {
	payload := map[string]string{"method": "mobile", "identifier": phone}
	body, status, err := makeHTTPCallWithHeaders(ctx, al.httpClient, "POST", "https://api.eka.care/abdm/na/v1/profile/login/init", payload, nil, abdmHeaders())
	if err != nil {
		return nil, fmt.Errorf("login init request failed: %v", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("login init failed: status %d", status)
	}

	var resp LoginInitResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("login init parse error: %v", err)
	}
	if resp.TxnID == "" {
		return nil, fmt.Errorf("login init returned empty txn_id")
	}
	return &resp, nil
}

func (al *ABDMLogin) VerifyOTP(ctx context.Context, txnID, otp string) (*LoginVerifyResponse, error) {
	payload := map[string]string{"txn_id": txnID, "otp": otp}
	body, status, err := makeHTTPCallWithHeaders(ctx, al.httpClient, "POST", "https://api.eka.care/abdm/na/v1/profile/login/verify", payload, nil, abdmHeaders())
	if err != nil {
		return nil, fmt.Errorf("verify request failed: %v", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("verify failed: status %d", status)
	}

	var resp LoginVerifyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("verify parse error: %v", err)
	}
	return &resp, nil
}

func (al *ABDMLogin) SelectPHR(ctx context.Context, txnID, phrAddress string) (*LoginPHRResponse, error) {
	payload := map[string]string{"txn_id": txnID, "phr_address": phrAddress}
	body, status, err := makeHTTPCallWithHeaders(ctx, al.httpClient, "POST", "https://api.eka.care/abdm/na/v1/profile/login/phr", payload, nil, abdmHeaders())
	if err != nil {
		return nil, fmt.Errorf("phr select request failed: %v", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("phr select failed: status %d", status)
	}

	var resp LoginPHRResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("phr select parse error: %v", err)
	}
	return &resp, nil
}

func (al *ABDMLogin) ExchangeMinToken(ctx context.Context, minToken, oid string) (string, string, error) {
	payload := map[string]string{
		"code":   minToken,
		"idp_id": "eka_min_token",
		"oid":    oid,
	}
	body, status, err := makeHTTPCallWithHeaders(ctx, al.httpClient, "POST", "https://aortago.eka.care/phr/v3/auth/verify", payload, nil, abdmHeaders())
	if err != nil {
		return "", "", fmt.Errorf("token exchange request failed: %v", err)
	}
	if status != 200 {
		return "", "", fmt.Errorf("token exchange failed: status %d", status)
	}

	var resp AortagoVerifyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", fmt.Errorf("token exchange parse error: %v", err)
	}
	sess := resp.Data.Tokens.Sess
	refresh := resp.Data.Tokens.Refresh
	if sess == "" || refresh == "" {
		return "", "", fmt.Errorf("token exchange returned empty sess/refresh")
	}
	return sess, refresh, nil
}

func makeHTTPCallWithHeaders(ctx context.Context, client HTTPClient, method, url string, payload interface{}, token *Token, extra map[string]string) ([]byte, int, error) {
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}

	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}

	headers := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   "EkaCare/3.1.3 (com.orbi.eka.care; build:1; iOS 19.0.0) Alamofire/5.10.2",
	}

	if token != nil {
		headers["auth"] = token.Auth
		headers["Cookie"] = fmt.Sprintf("sess=%s; refresh=%s", token.Sess, token.Refresh)
		headers["device-id"] = token.DeviceID
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := readLimitedResponse(resp, MaxResponseSize)
	return respBody, resp.StatusCode, err
}
