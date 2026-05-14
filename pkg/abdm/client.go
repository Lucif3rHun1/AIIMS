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
	"net/http"
	"net/http/cookiejar"
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
}

type defaultTokenManager struct {
	masterToken *Token
	httpClient  HTTPClient
	config      *config.Config
	mutex       sync.RWMutex
}

func NewTokenManager(cfg *config.Config, client HTTPClient, initialToken *Token) TokenManager {
	return &defaultTokenManager{
		masterToken: initialToken,
		httpClient:  client,
		config:      cfg,
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
		body, status, err := makeHTTPCall(context.Background(), tm.httpClient, "POST", "https://aortago.eka.care/phr/v3/auth/refresh", payload, tm.masterToken)
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

			deviceID, _ := generateSecureUUID()

			tm.masterToken = &Token{
				Auth:      sess,
				Sess:      sess,
				Refresh:   refresh,
				DeviceID:  deviceID,
				ExpiresAt: parseJWTExpiry(sess),
			}
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

	body, status, err := makeHTTPCall(context.Background(), tm.httpClient, "POST", "https://aortago.eka.care/v3/auth/switch", payload, masterToken)
	if err != nil || status != 200 {
		return nil, fmt.Errorf("switch failed: %v (status %d)", err, status)
	}

	var switchResponse map[string]string
	if err := json.Unmarshal(body, &switchResponse); err != nil {
		return nil, err
	}

	sess := switchResponse["sess"]
	refresh := switchResponse["refresh"]

	deviceID, _ := generateSecureUUID()

	return &Token{
		Auth:      sess,
		Sess:      sess,
		Refresh:   refresh,
		DeviceID:  deviceID,
		ExpiresAt: parseJWTExpiry(sess),
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
		"flavour":      "ios",
		"version":      "3.1.3",
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

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := readLimitedResponse(resp, MaxResponseSize)
	return respBody, resp.StatusCode, err
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

func generateSecureUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func parseJWTExpiry(token string) time.Time {
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

func ParseJWTExpiry(token string) time.Time {
	return parseJWTExpiry(token)
}

func GenerateSecureUUID() (string, error) {
	return generateSecureUUID()
}
