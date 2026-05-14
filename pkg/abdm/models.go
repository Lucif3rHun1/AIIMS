package abdm

import (
	"time"
)

type Token struct {
	Auth      string
	Sess      string
	Refresh   string
	DeviceID  string
	ExpiresAt time.Time
}

func (t *Token) IsExpired() bool {
	return t.ExpiresAt.Before(time.Now().Add(30 * time.Minute))
}

type Patient struct {
	OID       string   `json:"oid"`
	FLN       string   `json:"fln"`
	HealthIDs []string `json:"health-ids"`
	ABHA      string   `json:"abha"`
}

func (p *Patient) PrimaryHealthID() string {
	if len(p.HealthIDs) > 0 {
		return p.HealthIDs[0]
	}
	return ""
}

type ValidatedPatient struct {
	Patient        Patient
	ValidationDone bool
}

type BurstResult struct {
	Attempt      int    `json:"attempt"`
	Timestamp    string `json:"timestamp"`
	Status       string `json:"status"`
	ResponseTime int64  `json:"response_time_ms"`
	TokenNumber  string `json:"token_number,omitempty"`
	Error        string `json:"error,omitempty"`
}

type PatientSummary struct {
	Patient       Patient `json:"patient"`
	TotalAttempts int     `json:"total_attempts"`
	Successful    int     `json:"successful"`
	ValidationOK  bool    `json:"validation_success"`
	TokenNumber   string  `json:"token_number,omitempty"`
	ExecutionTime string  `json:"execution_time"`
	GoldenAttempt int     `json:"golden_attempt,omitempty"`
}

func jsonMapsEqual(m1, m2 map[string]interface{}) bool {
	if len(m1) != len(m2) {
		return false
	}

	for k, v1 := range m1 {
		v2, exists := m2[k]
		if !exists {
			return false
		}

		switch v1Type := v1.(type) {
		case map[string]interface{}:
			v2Map, ok := v2.(map[string]interface{})
			if !ok || !jsonMapsEqual(v1Type, v2Map) {
				return false
			}
		case []interface{}:
			v2Slice, ok := v2.([]interface{})
			if !ok || !jsonSlicesEqual(v1Type, v2Slice) {
				return false
			}
		default:
			if v1 != v2 { // Simple comparison for primitives
				return false
			}
		}
	}
	return true
}

func jsonValuesEqual(v1, v2 interface{}) bool {
	switch v1Val := v1.(type) {
	case map[string]interface{}:
		v2Map, ok := v2.(map[string]interface{})
		if !ok {
			return false
		}
		return jsonMapsEqual(v1Val, v2Map)
	case []interface{}:
		v2Slice, ok := v2.([]interface{})
		if !ok {
			return false
		}
		return jsonSlicesEqual(v1Val, v2Slice)
	case float64:
		v2Float, ok := v2.(float64)
		return ok && v1Val == v2Float
	case string:
		v2Str, ok := v2.(string)
		return ok && v1Val == v2Str
	case bool:
		v2Bool, ok := v2.(bool)
		return ok && v1Val == v2Bool
	case nil:
		return v2 == nil
	default:
		return v1 == v2
	}
}

func jsonSlicesEqual(s1, s2 []interface{}) bool {
	if len(s1) != len(s2) {
		return false
	}
	for i := range s1 {
		if !jsonValuesEqual(s1[i], s2[i]) {
			return false
		}
	}
	return true
}
