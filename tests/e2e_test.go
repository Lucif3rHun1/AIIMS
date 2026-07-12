package tests

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aiims-appointment/pkg/abdm"
	"aiims-appointment/pkg/config"
	"aiims-appointment/pkg/crypto"
	"aiims-appointment/pkg/notify"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ============================================================
// SECTION 1: CONFIG TESTS
// ============================================================

func TestConfigLoadAndValidate(t *testing.T) {
	tmpFile := "test_config_temp.json"
	defer os.Remove(tmpFile)

	cfg := &config.Config{
		AuthToken:        "test_jwt_token",
		RefreshToken:     "test_refresh",
		HipID:            "IN2210000099",
		TargetDate:       "2026-05-18",
		Timezone:         "Asia/Kolkata",
		PhoneNumber:      "9471392919",
		TelegramBotToken: "123456:ABC-DEF",
		OwnerID:          5016416878,
		BroadcastChatID:  0,
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(tmpFile, data, 0644)

	loaded, err := config.LoadConfig(tmpFile)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if loaded.HipID != "IN2210000099" {
		t.Errorf("HipID mismatch: got %s", loaded.HipID)
	}
	if loaded.OwnerID != 5016416878 {
		t.Errorf("OwnerID mismatch: got %d", loaded.OwnerID)
	}
	if loaded.PhoneNumber != "9471392919" {
		t.Errorf("PhoneNumber mismatch: got %s", loaded.PhoneNumber)
	}
	if loaded.TelegramBotToken != "123456:ABC-DEF" {
		t.Errorf("BotToken mismatch: got %s", loaded.TelegramBotToken)
	}

	if err := loaded.Validate(); err != nil {
		t.Errorf("Validate failed: %v", err)
	}
}

func TestConfigValidate_MissingFields(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(c *config.Config)
		wantErr string
	}{
		{"missing bot_token", func(c *config.Config) { c.TelegramBotToken = "" }, "telegram_bot_token"},
		{"missing owner_id", func(c *config.Config) { c.OwnerID = 0 }, "owner_id"},
		{"invalid timezone", func(c *config.Config) { c.Timezone = "Invalid/Zone" }, "timezone"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				AuthToken:        "token",
				RefreshToken:     "refresh",
				HipID:            "HIP123",
				TargetDate:       "2026-05-18",
				Timezone:         "Asia/Kolkata",
				TelegramBotToken: "123:ABC",
				OwnerID:          123,
			}
			tt.modify(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantErr)
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestConfigSaveConcurrent(t *testing.T) {
	tmpFile := "test_config_concurrent.json"
	defer os.Remove(tmpFile)

	cfg := &config.Config{
		AuthToken:        "token",
		RefreshToken:     "refresh",
		HipID:            "HIP123",
		TelegramBotToken: "123:ABC",
		OwnerID:          123,
	}

	var wg sync.WaitGroup
	var errors atomic.Int32

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg.UpdateTokens(fmt.Sprintf("token_%d", i), fmt.Sprintf("refresh_%d", i))
			if err := cfg.Save(tmpFile); err != nil {
				errors.Add(1)
				t.Logf("Save error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if errors.Load() > 0 {
		t.Errorf("%d concurrent save errors", errors.Load())
	}

	// Verify file is valid JSON
	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("Failed to read saved config: %v", err)
	}
	var loaded config.Config
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("Saved config is invalid JSON: %v", err)
	}
}

func TestConfigIsOwner(t *testing.T) {
	cfg := &config.Config{OwnerID: 5016416878}
	if !cfg.IsOwner(5016416878) {
		t.Error("IsOwner should return true for owner")
	}
	if cfg.IsOwner(12345) {
		t.Error("IsOwner should return false for non-owner")
	}
}

func TestConfigBroadcastChannel(t *testing.T) {
	cfg := &config.Config{BroadcastChatID: 0}
	if cfg.HasBroadcastChannel() {
		t.Error("Should not have broadcast channel when 0")
	}
	cfg.SetBroadcastChatID(-100123456)
	if !cfg.HasBroadcastChannel() {
		t.Error("Should have broadcast channel when non-zero")
	}
}

func TestConfigRedact(t *testing.T) {
	cfg := &config.Config{
		TelegramBotToken: "1234567890:ABCDEFghijklmnop",
		OwnerID:          5016416878,
		BroadcastChatID:  -100123,
		HipID:            "IN2210000099",
	}
	redacted := cfg.Redact()
	if strings.Contains(redacted, "ABCDEFghijklmnop") {
		t.Error("Redact should not contain full bot token")
	}
	if !strings.Contains(redacted, "IN2210000099") {
		t.Error("Redact should contain HIP ID")
	}
}

func TestConfigUpdateTokens(t *testing.T) {
	cfg := &config.Config{
		AuthToken:    "old_sess",
		RefreshToken: "old_refresh",
	}
	cfg.UpdateTokens("new_sess", "new_refresh")
	if cfg.AuthToken != "new_sess" {
		t.Error("AuthToken not updated")
	}
	if cfg.RefreshToken != "new_refresh" {
		t.Error("RefreshToken not updated")
	}
}

// ============================================================
// SECTION 2: TOKEN / MODEL TESTS
// ============================================================

func TestTokenIsExpired(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"zero time", time.Time{}, true},
		{"already expired", time.Now().Add(-1 * time.Hour), true},
		{"expires in 4 minutes (within 5min buffer)", time.Now().Add(4 * time.Minute), true},
		{"expires in 6 minutes (outside buffer)", time.Now().Add(6 * time.Minute), false},
		{"expires in 1 hour", time.Now().Add(1 * time.Hour), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok := &abdm.Token{ExpiresAt: tt.expiresAt}
			if got := tok.IsExpired(); got != tt.want {
				t.Errorf("IsExpired() = %v, want %v (expiresAt = %v)", got, tt.want, tt.expiresAt)
			}
		})
	}
}

func TestParseJWTExpiry(t *testing.T) {
	// Create a fake JWT with known expiry
	// Header: {"alg":"HS256","typ":"JWT"}
	// Payload: {"exp":1778780795}
	header := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
	payload := "eyJleHAiOjE3Nzg3ODA3OTV9"
	signature := "fakesig"
	fakeJWT := header + "." + payload + "." + signature

	expiry := abdm.ParseJWTExpiry(fakeJWT)
	if expiry.IsZero() {
		t.Error("ParseJWTExpiry returned zero time for valid JWT")
	}
	expected := time.Unix(1778780795, 0)
	if !expiry.Equal(expected) {
		t.Errorf("ParseJWTExpiry = %v, want %v", expiry, expected)
	}
}

func TestParseJWTExpiry_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"no dots", "abc"},
		{"one dot", "a.b"},
		{"invalid base64", "a.!!!.c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expiry := abdm.ParseJWTExpiry(tt.token)
			if !expiry.IsZero() {
				t.Errorf("expected zero time for %q, got %v", tt.token, expiry)
			}
		})
	}
}

func TestPatientPrimaryHealthID(t *testing.T) {
	tests := []struct {
		name    string
		patient abdm.Patient
		want    string
	}{
		{"has health IDs", abdm.Patient{HealthIDs: []string{"hid1@abdm", "hid2@abdm"}}, "hid1@abdm"},
		{"empty health IDs", abdm.Patient{HealthIDs: []string{}}, ""},
		{"nil health IDs", abdm.Patient{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.patient.PrimaryHealthID(); got != tt.want {
				t.Errorf("PrimaryHealthID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateSecureUUID(t *testing.T) {
	uuid1, err := abdm.GenerateSecureUUID()
	if err != nil {
		t.Fatalf("GenerateSecureUUID failed: %v", err)
	}
	if uuid1 == "" {
		t.Error("UUID should not be empty")
	}

	uuid2, _ := abdm.GenerateSecureUUID()
	if uuid1 == uuid2 {
		t.Error("Two UUIDs should not be equal")
	}

	// UUID format: XX-XX-XX-XX-XXXXXXXXXXXX (5 groups separated by -)
	parts := strings.Split(uuid1, "-")
	if len(parts) != 5 {
		t.Errorf("UUID should have 5 parts, got %d: %s", len(parts), uuid1)
	}
}

// ============================================================
// SECTION 3: NOTIFIER FORMAT TESTS
// ============================================================

func TestFormatAppointmentSuccess(t *testing.T) {
	result := notify.FormatAppointmentSuccess(
		"John O'Brien", "health@abdm", "T-042",
		"AIIMS Raipur", "06:00 AM", "Proceed to counter",
	)

	if !strings.Contains(result, "✅") {
		t.Error("Missing checkmark")
	}
	if !strings.Contains(result, "<b>Appointment Confirmed</b>") {
		t.Error("Missing bold header")
	}
	if !strings.Contains(result, "O&#39;Brien") {
		t.Errorf("HTML escaping failed for patient name: %s", result)
	}
	if !strings.Contains(result, "<code>T-042</code>") {
		t.Error("Token number not in code tags")
	}
}

func TestFormatQueueUpdate(t *testing.T) {
	result := notify.FormatQueueUpdate("Test Patient", 3, 10)
	if !strings.Contains(result, "3 / 10") {
		t.Errorf("Queue position not formatted correctly: %s", result)
	}
	if !strings.Contains(result, "<code>Test Patient</code>") {
		t.Error("Patient name not in code tags")
	}
}

func TestFormatValidationStatus_Success(t *testing.T) {
	result := notify.FormatValidationStatus("Patient A", 2, 5, "")
	if !strings.Contains(result, "🔐") {
		t.Error("Missing validation icon")
	}
	if !strings.Contains(result, "2 / 5") {
		t.Errorf("Progress not shown: %s", result)
	}
}

func TestFormatValidationStatus_Failure(t *testing.T) {
	result := notify.FormatValidationStatus("Patient B", 0, 0, "Token switch failed")
	if !strings.Contains(result, "❌") {
		t.Error("Missing failure icon")
	}
	if !strings.Contains(result, "Token switch failed") {
		t.Error("Error message missing")
	}
}

func TestFormatBurstResult_Success(t *testing.T) {
	result := notify.FormatBurstResult("Patient C", true, "T-123")
	if !strings.Contains(result, "✅") {
		t.Error("Missing success icon")
	}
	if !strings.Contains(result, "T-123") {
		t.Error("Missing token number")
	}
}

func TestFormatBurstResult_Failure(t *testing.T) {
	result := notify.FormatBurstResult("Patient D", false, "")
	if !strings.Contains(result, "❌") {
		t.Error("Missing failure icon")
	}
	if !strings.Contains(result, "exhausted") {
		t.Error("Missing exhaustion message")
	}
}

func TestFormatExecutionStart(t *testing.T) {
	result := notify.FormatExecutionStart(5, "18 May 2026 06:00 IST")
	if !strings.Contains(result, "🚀") {
		t.Error("Missing rocket icon")
	}
	if !strings.Contains(result, "5") {
		t.Error("Missing patient count")
	}
}

func TestFormatExecutionComplete(t *testing.T) {
	tests := []struct {
		name    string
		success int32
		fail    int32
		total   int
	}{
		{"normal", 3, 2, 5},
		{"all success", 5, 0, 5},
		{"all failed", 0, 5, 5},
		{"zero total (NaN guard)", 0, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := notify.FormatExecutionComplete(tt.success, tt.fail, tt.total)
			if !strings.Contains(result, "📊") {
				t.Error("Missing chart icon")
			}
			if strings.Contains(result, "NaN") {
				t.Errorf("Output contains NaN: %s", result)
			}
		})
	}
}

func TestBuildStatusMessage(t *testing.T) {
	idle := notify.BuildStatusMessage(0, 0, 0, 0, false)
	if !strings.Contains(idle, "Idle") {
		t.Errorf("Idle message wrong: %s", idle)
	}

	running := notify.BuildStatusMessage(3, 5, 1, 1, true)
	if !strings.Contains(running, "Task Running") {
		t.Errorf("Running message wrong: %s", running)
	}
	if !strings.Contains(running, "3 / 5") {
		t.Errorf("Progress not shown: %s", running)
	}
}

func TestHTMLEscapeInFormats(t *testing.T) {
	result := notify.FormatAppointmentSuccess(
		"<script>alert('xss')</script>", "test@abdm", "T-1",
		"Hospital", "", "",
	)
	if strings.Contains(result, "<script>") {
		t.Errorf("XSS not escaped: %s", result)
	}
	if !strings.Contains(result, "&lt;script&gt;") {
		t.Errorf("HTML not properly escaped: %s", result)
	}
}

// ============================================================
// SECTION 4: MANAGER TESTS
// ============================================================

func TestManagerGetValidatedPool_DefensiveCopy(t *testing.T) {
	cfg := &config.Config{
		AuthToken:    "token",
		RefreshToken: "refresh",
		HipID:        "HIP123",
	}
	mgr := abdm.NewABDMManager(cfg, context.Background())
	defer mgr.Close()

	pool1 := mgr.GetValidatedPool()
	pool2 := mgr.GetValidatedPool()
	if len(pool1) != len(pool2) {
		t.Errorf("Pool lengths differ: %d vs %d", len(pool1), len(pool2))
	}
}

func TestManagerClose_WaitsForGoroutines(t *testing.T) {
	cfg := &config.Config{
		AuthToken:    "token",
		RefreshToken: "refresh",
		HipID:        "HIP123",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := abdm.NewABDMManager(cfg, ctx)
	done := make(chan bool)
	go func() {
		mgr.Close()
		done <- true
	}()

	select {
	case <-done:
		// Success
	case <-time.After(15 * time.Second):
		t.Fatal("Manager Close hung for >15s")
	}
}

// ============================================================
// SECTION 5: TG BOT API LIVE TESTS
// ============================================================

func getBotToken(t *testing.T) string {
	t.Helper()
	paths := []string{"config.json", "../config.json"}
	var data []byte
	for _, p := range paths {
		if d, err := os.ReadFile(p); err == nil {
			data = d
			break
		}
	}
	if data == nil {
		t.Skip("No config.json found — skipping live TG tests")
	}
	var cfg map[string]interface{}
	json.Unmarshal(data, &cfg)
	if token, ok := cfg["telegram_bot_token"].(string); ok && token != "" {
		return token
	}
	t.Skip("No telegram_bot_token in config.json")
	return ""
}

func getOwnerChatID(t *testing.T) int64 {
	t.Helper()
	paths := []string{"config.json", "../config.json"}
	var data []byte
	for _, p := range paths {
		if d, err := os.ReadFile(p); err == nil {
			data = d
			break
		}
	}
	if data == nil {
		t.Skip("No config.json found — skipping")
	}
	var cfg map[string]interface{}
	json.Unmarshal(data, &cfg)
	if id, ok := cfg["owner_id"].(float64); ok {
		return int64(id)
	}
	t.Skip("No owner_id in config")
	return 0
}

func TestTGBotAPI_Connectivity(t *testing.T) {
	token := getBotToken(t)
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		t.Fatalf("Failed to connect to TG Bot API: %v", err)
	}
	t.Logf("Connected as @%s (ID: %d)", bot.Self.UserName, bot.Self.ID)

	if bot.Self.UserName == "" {
		t.Error("Bot username is empty")
	}
}

func TestTGBotAPI_SendTestMessage(t *testing.T) {
	token := getBotToken(t)
	chatID := getOwnerChatID(t)

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		t.Fatalf("Bot API connection failed: %v", err)
	}

	msg := tgbotapi.NewMessage(chatID, "🧪 <b>E2E Test</b> — Bot connectivity verified.\n<i>Safe to ignore.</i>")
	msg.ParseMode = tgbotapi.ModeHTML

	sent, err := bot.Send(msg)
	if err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}
	t.Logf("Message sent successfully (msg_id: %d)", sent.MessageID)
}

func TestTGBotAPI_SendHTML(t *testing.T) {
	token := getBotToken(t)
	chatID := getOwnerChatID(t)

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		t.Fatalf("Bot API connection failed: %v", err)
	}

	htmlMsg := notify.FormatAppointmentSuccess(
		"Test Patient O'Brien", "test@abdm", "T-042",
		"AIIMS Raipur", "06:00 AM", "Proceed to counter",
	)

	msg := tgbotapi.NewMessage(chatID, htmlMsg)
	msg.ParseMode = tgbotapi.ModeHTML

	sent, err := bot.Send(msg)
	if err != nil {
		t.Fatalf("Failed to send HTML message: %v", err)
	}
	t.Logf("HTML message sent (msg_id: %d)", sent.MessageID)
}

func TestTGBotAPI_SendExecutionComplete(t *testing.T) {
	token := getBotToken(t)
	chatID := getOwnerChatID(t)

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		t.Fatalf("Bot API connection failed: %v", err)
	}

	htmlMsg := notify.FormatExecutionComplete(3, 2, 5)
	msg := tgbotapi.NewMessage(chatID, htmlMsg)
	msg.ParseMode = tgbotapi.ModeHTML

	sent, err := bot.Send(msg)
	if err != nil {
		t.Fatalf("Failed to send execution complete: %v", err)
	}
	t.Logf("Execution complete sent (msg_id: %d)", sent.MessageID)
}

func TestTGBotAPI_SendInlineKeyboard(t *testing.T) {
	token := getBotToken(t)
	chatID := getOwnerChatID(t)

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		t.Fatalf("Bot API connection failed: %v", err)
	}

	msg := tgbotapi.NewMessage(chatID, "🧪 Test: Main Menu Keyboard")
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Fetch Patients", "cmd_fetch"),
			tgbotapi.NewInlineKeyboardButtonData("📊 Status", "cmd_status"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("▶️ Run Task", "cmd_run"),
			tgbotapi.NewInlineKeyboardButtonData("🛑 Stop", "cmd_stop"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", "cmd_settings"),
		),
	)

	sent, err := bot.Send(msg)
	if err != nil {
		t.Fatalf("Failed to send inline keyboard: %v", err)
	}
	t.Logf("Keyboard sent (msg_id: %d)", sent.MessageID)
}

func TestTGBotAPI_SendDatePickerKeyboard(t *testing.T) {
	token := getBotToken(t)
	chatID := getOwnerChatID(t)

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		t.Fatalf("Bot API connection failed: %v", err)
	}

	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := time.Now().In(loc)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)

	rows := make([][]tgbotapi.InlineKeyboardButton, 0, 5)
	for row := 0; row < 4; row++ {
		r := make([]tgbotapi.InlineKeyboardButton, 0, 2)
		for col := 0; col < 2; col++ {
			offset := row*2 + col
			d := today.AddDate(0, 0, offset)
			dayLabel := d.Format("02 Jan (Mon)")
			if offset == 0 {
				dayLabel = "📌 Today " + d.Format("02 Jan")
			} else if offset == 1 {
				dayLabel = "📅 Tomorrow " + d.Format("02 Jan")
			}
			r = append(r, tgbotapi.NewInlineKeyboardButtonData(dayLabel, fmt.Sprintf("date_%d", offset)))
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(r...))
	}
	rows = append(rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📝 Custom Date (DD-MM-YYYY)", "date_custom"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Cancel", "cmd_start"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "🧪 Test: Date Picker Keyboard")
	msg.ReplyMarkup = tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}

	sent, err := bot.Send(msg)
	if err != nil {
		t.Fatalf("Failed to send date picker: %v", err)
	}
	t.Logf("Date picker sent (msg_id: %d)", sent.MessageID)
}

func TestTGBotAPI_NotifierRetry(t *testing.T) {
	token := getBotToken(t)
	chatID := getOwnerChatID(t)

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		t.Fatalf("Bot API connection failed: %v", err)
	}

	notifier := notify.NewNotifier(bot, chatID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := notifier.SendHTML(ctx, "🧪 <b>Notifier Retry Test</b> — Direct send via Notifier interface"); err != nil {
		t.Fatalf("Notifier.SendHTML failed: %v", err)
	}
	t.Log("Notifier.SendHTML succeeded")
}

func TestTGBotAPI_NotifierNoChannel(t *testing.T) {
	token := getBotToken(t)

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		t.Fatalf("Bot API connection failed: %v", err)
	}

	notifier := notify.NewNotifier(bot, 0)

	if notifier.HasChannel() {
		t.Error("HasChannel should be false with chatID=0")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = notifier.SendHTML(ctx, "should not send")
	if err == nil {
		t.Error("Send should fail with no channel configured")
	}
	if !strings.Contains(err.Error(), "no broadcast channel") {
		t.Errorf("Unexpected error: %v", err)
	}
}

// ============================================================
// SECTION 6: EKA.CARE API LIVE TESTS
// ============================================================

func TestEkaCare_RefreshEndpoint(t *testing.T) {
	data := readConfigJSON(t)
	if data == nil {
		return
	}
	var cfg map[string]interface{}
	json.Unmarshal(data, &cfg)

	sess, _ := cfg["auth_token"].(string)
	refresh, _ := cfg["refresh_token"].(string)
	if sess == "" || refresh == "" {
		t.Skip("No auth tokens in config")
	}

	body := fmt.Sprintf(`{"sess":"%s","refresh":"%s"}`, sess, refresh)
	req, err := http.NewRequest("POST", "https://aortago.eka.care/phr/v3/auth/refresh", strings.NewReader(body))
	if err != nil {
		t.Skipf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("client-id", "patient-app-ios")
	req.Header.Set("Origin", "file://")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("Network error (eka.care unreachable): %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	t.Logf("Refresh endpoint returned status %d", resp.StatusCode)
	t.Logf("Response: %s", truncateStr(string(respBody), 300))
	if resp.StatusCode == 403 {
		t.Log("Refresh token stale — login flow needed (expected)")
	}
	if resp.StatusCode == 200 {
		t.Log("✅ Token refresh succeeded!")
	}
}

// ============================================================
// SECTION 7: INTEGRATION FLOW TESTS
// ============================================================

func TestFullConfigRoundTrip(t *testing.T) {
	tmpFile := "test_roundtrip.json"
	defer os.Remove(tmpFile)

	original := &config.Config{
		AuthToken:        "test_sess",
		RefreshToken:     "test_refresh",
		HipID:            "IN2210000099",
		TargetDate:       "2026-05-20",
		Timezone:         "Asia/Kolkata",
		PhoneNumber:      "9471392919",
		TelegramBotToken: "123456:ABC",
		OwnerID:          5016416878,
		BroadcastChatID:  -100123,
		AuthorizedUsers:  []string{"user1", "user2"},
	}

	if err := original.Save(tmpFile); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := config.LoadConfig(tmpFile)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if loaded.AuthToken != original.AuthToken {
		t.Errorf("AuthToken mismatch")
	}
	if loaded.RefreshToken != original.RefreshToken {
		t.Errorf("RefreshToken mismatch")
	}
	if loaded.HipID != original.HipID {
		t.Errorf("HipID mismatch")
	}
	if loaded.TargetDate != original.TargetDate {
		t.Errorf("TargetDate mismatch")
	}
	if loaded.PhoneNumber != original.PhoneNumber {
		t.Errorf("PhoneNumber mismatch")
	}
	if loaded.TelegramBotToken != original.TelegramBotToken {
		t.Errorf("TelegramBotToken mismatch")
	}
	if loaded.OwnerID != original.OwnerID {
		t.Errorf("OwnerID mismatch")
	}
	if loaded.BroadcastChatID != original.BroadcastChatID {
		t.Errorf("BroadcastChatID mismatch")
	}
	if len(loaded.AuthorizedUsers) != len(original.AuthorizedUsers) {
		t.Errorf("AuthorizedUsers length mismatch")
	}
}

func TestDatePickerISTTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("Failed to load IST timezone: %v", err)
	}

	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	for offset := 0; offset < 8; offset++ {
		d := today.AddDate(0, 0, offset)
		if d.Location().String() != "Asia/Kolkata" {
			t.Errorf("Date %d not in IST: %v", offset, d.Location())
		}
	}
}

func TestTargetTimeCalculation(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")

	parsed, _ := time.Parse("02-01-2006", "18-05-2026")
	targetDate := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, loc)
	target := time.Date(
		targetDate.Year(), targetDate.Month(), targetDate.Day(),
		6, 0, 0, 0, loc,
	)

	if target.Hour() != 6 {
		t.Errorf("Target hour = %d, want 6", target.Hour())
	}
	if target.Location().String() != "Asia/Kolkata" {
		t.Errorf("Target timezone = %s, want Asia/Kolkata", target.Location())
	}
}

// ============================================================
// SECTION 8: RACE CONDITION VERIFICATION
// ============================================================

func TestConcurrentConfigAccess(t *testing.T) {
	cfg := &config.Config{
		AuthToken:    "token",
		RefreshToken: "refresh",
		HipID:        "HIP123",
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			cfg.IsOwner(int64(i))
		}(i)
		go func(i int) {
			defer wg.Done()
			cfg.SetBroadcastChatID(int64(i))
		}(i)
		go func(i int) {
			defer wg.Done()
			cfg.UpdateTokens(fmt.Sprintf("sess_%d", i), fmt.Sprintf("ref_%d", i))
		}(i)
	}
	wg.Wait()
}

func TestConcurrentSelectedIDs(t *testing.T) {
	selected := make(map[string]bool)
	var mu sync.RWMutex

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			mu.Lock()
			selected[fmt.Sprintf("oid_%d", i)] = true
			mu.Unlock()
		}(i)
		go func() {
			defer wg.Done()
			mu.RLock()
			_ = len(selected)
			mu.RUnlock()
		}()
	}
	wg.Wait()

	if len(selected) != 100 {
		t.Errorf("Expected 100 selected, got %d", len(selected))
	}
}

// ============================================================
// SECTION 9: HTML ESCAPING VERIFICATION
// ============================================================

func TestAllFormatFunctions_HTMLSafe(t *testing.T) {
	evilName := `<b>evil</b>"onmouseover="alert(1)`
	evilToken := `<script>token</script>`
	evilHIP := `Hospital&Clinic<`

	formats := []string{
		notify.FormatAppointmentSuccess(evilName, "hid", evilToken, evilHIP, "", ""),
		notify.FormatQueueUpdate(evilName, 1, 10),
		notify.FormatValidationStatus(evilName, 1, 5, evilHIP),
		notify.FormatBurstResult(evilName, false, ""),
		notify.FormatBurstResult(evilName, true, evilToken),
	}

	evilEscaped := html.EscapeString(evilName)
	for i, f := range formats {
		if strings.Contains(f, "<script>") {
			t.Errorf("Format %d contains unescaped <script>: %s", i, f)
		}
		if !strings.Contains(f, evilEscaped) {
			t.Errorf("Format %d did not properly escape evil name\nGot: %s\nWant substring: %s", i, f, evilEscaped)
		}
	}
}

// ============================================================
// SECTION 10: CONTEXT CANCELLATION TESTS
// ============================================================

func TestContextCancellation_Select(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			done <- ctx.Err()
		case <-time.After(10 * time.Second):
			done <- nil
		}
	}()

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Should have received context error")
		}
	case <-time.After(2 * time.Second):
		t.Error("Context cancellation took too long")
	}
}

func TestBufferedChannelNonBlocking(t *testing.T) {
	ch := make(chan string, 1)

	select {
	case ch <- "test":
	default:
		t.Error("Buffered channel should accept first value")
	}

	select {
	case ch <- "overflow":
		t.Error("Should have hit default case")
	default:
		// Expected
	}

	select {
	case v := <-ch:
		if v != "test" {
			t.Errorf("Expected 'test', got %q", v)
		}
	default:
		t.Error("Should have gotten value")
	}
}

// ============================================================
// SECTION 11: EXTRACT TOKEN NUMBER
// ============================================================

func TestExtractTokenNumber(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"token_number at root", `{"token_number":"T-042"}`, "T-042"},
		{"token_number in data", `{"data":{"token_number":"T-123"}}`, "T-123"},
		{"no token_number", `{"status":"ok"}`, ""},
		{"empty json", `{}`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m map[string]interface{}
			json.Unmarshal([]byte(tt.json), &m)

			var got string
			if t, ok := m["token_number"].(string); ok {
				got = t
			} else if d, ok := m["data"].(map[string]interface{}); ok {
				if t, ok := d["token_number"].(string); ok {
					got = t
				}
			}

			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ============================================================
// SECTION 12: PRE-WARM TIMING MATH
// ============================================================

func TestPreWarmTimingMath(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")

	// Target: today at 6:00 AM IST
	now := time.Now().In(loc)
	target := time.Date(now.Year(), now.Month(), now.Day(), 6, 0, 0, 0, loc)

	preWarmOffset := 30 * time.Second
	preWarmTime := target.Add(-preWarmOffset)

	wait := time.Until(target)
	preWarmWait := time.Until(preWarmTime)

	t.Logf("Target: %s", target.Format("15:04:05 MST"))
	t.Logf("Pre-warm time: %s", preWarmTime.Format("15:04:05 MST"))
	t.Logf("Wait: %v", wait.Round(time.Second))
	t.Logf("Pre-warm wait: %v", preWarmWait.Round(time.Second))

	// Pre-warm should always be 30s before target
	diff := target.Sub(preWarmTime)
	if diff != preWarmOffset {
		t.Errorf("Pre-warm offset = %v, want %v", diff, preWarmOffset)
	}
}

// ============================================================
// SECTION 13: MATH UTILS
// ============================================================

func TestExecutionCompleteRate(t *testing.T) {
	// Verify no NaN for all edge cases
	tests := []struct {
		success int32
		fail    int32
		total   int
	}{
		{3, 2, 5},
		{0, 0, 0},
		{0, 5, 5},
		{5, 0, 5},
		{1, 0, 0}, // inconsistent but shouldn't crash
	}

	for _, tt := range tests {
		rate := float64(0)
		if tt.total > 0 {
			rate = float64(tt.success) / float64(tt.total) * 100
		}
		if math.IsNaN(rate) {
			t.Errorf("NaN rate for %+v", tt)
		}
		if math.IsInf(rate, 0) {
			t.Errorf("Inf rate for %+v", tt)
		}
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func readConfigJSON(t *testing.T) []byte {
	t.Helper()
	paths := []string{"config.json", "../config.json"}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return data
		}
	}
	t.Skip("No config.json found")
	return nil
}

// ============================================================
// SECTION 14: CRYPTO & ENCRYPTION TESTS
// ============================================================

// testMasterKey generates a deterministic 32-byte AES-256 key encoded as base64.
func testMasterKey() string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestCryptoRoundTrip(t *testing.T) {
	mk := testMasterKey()

	t.Run("encrypt_decrypt_roundtrip", func(t *testing.T) {
		t.Setenv(crypto.EnvMasterKey, mk)

		plaintext := "my-secret-token-12345"
		encrypted, err := crypto.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt failed: %v", err)
		}
		if encrypted == plaintext {
			t.Error("Encrypted value should differ from plaintext")
		}

		decrypted, err := crypto.Decrypt(encrypted)
		if err != nil {
			t.Fatalf("Decrypt failed: %v", err)
		}
		if decrypted != plaintext {
			t.Errorf("Decrypted = %q, want %q", decrypted, plaintext)
		}
	})

	t.Run("encrypt_no_key_returns_plaintext", func(t *testing.T) {
		t.Setenv(crypto.EnvMasterKey, "")

		result, err := crypto.Encrypt("plain-value")
		if err != nil {
			t.Fatalf("Encrypt without key failed: %v", err)
		}
		if result != "plain-value" {
			t.Errorf("Encrypt without key = %q, want %q", result, "plain-value")
		}
	})

	t.Run("decrypt_non_base64_returns_input", func(t *testing.T) {
		t.Setenv(crypto.EnvMasterKey, mk)

		notBase64 := "this-is-not-base64!!!"
		decrypted, err := crypto.Decrypt(notBase64)
		if err != nil {
			t.Fatalf("Decrypt non-base64 failed: %v", err)
		}
		if decrypted != notBase64 {
			t.Errorf("Decrypt non-base64 = %q, want %q", decrypted, notBase64)
		}
	})

	t.Run("secure_wipe_zeros_bytes", func(t *testing.T) {
		data := []byte("sensitive-data")
		crypto.SecureWipe(data)
		for i, b := range data {
			if b != 0 {
				t.Errorf("SecureWipe: byte %d = %d, want 0", i, b)
			}
		}
	})
}

func TestConfigEncryptionRoundTrip(t *testing.T) {
	mk := testMasterKey()
	t.Setenv(crypto.EnvMasterKey, mk)

	tmpDir := t.TempDir()
	cfgPath := tmpDir + "/config.json"

	origAuth := "plaintext-auth-token-xyz"
	origRefresh := "plaintext-refresh-token-abc"

	cfg := &config.Config{
		TelegramBotToken: "123:ABC",
		OwnerID:          123,
		Accounts: map[string]*config.Account{
			"acc1": {
				ID:           "acc1",
				PhoneNumber:  "9471392919",
				AuthToken:    origAuth,
				RefreshToken: origRefresh,
			},
		},
	}

	// Save — tokens should be encrypted on disk but restored in memory
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// In-memory tokens must remain plaintext (Save calls decryptTokens in defer)
	if cfg.Accounts["acc1"].AuthToken != origAuth {
		t.Errorf("In-memory AuthToken changed after Save: got %q", cfg.Accounts["acc1"].AuthToken)
	}
	if cfg.Accounts["acc1"].RefreshToken != origRefresh {
		t.Errorf("In-memory RefreshToken changed after Save: got %q", cfg.Accounts["acc1"].RefreshToken)
	}

	// Read raw file — tokens must NOT be plaintext
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("Failed to read raw config: %v", err)
	}
	rawStr := string(raw)
	if strings.Contains(rawStr, origAuth) {
		t.Error("Raw config file contains plaintext auth_token — encryption failed")
	}
	if strings.Contains(rawStr, origRefresh) {
		t.Error("Raw config file contains plaintext refresh_token — encryption failed")
	}

	// LoadConfig should decrypt and restore original tokens
	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if loaded.Accounts["acc1"].AuthToken != origAuth {
		t.Errorf("Loaded AuthToken = %q, want %q", loaded.Accounts["acc1"].AuthToken, origAuth)
	}
	if loaded.Accounts["acc1"].RefreshToken != origRefresh {
		t.Errorf("Loaded RefreshToken = %q, want %q", loaded.Accounts["acc1"].RefreshToken, origRefresh)
	}
}

func TestMultiAccountConcurrentSave(t *testing.T) {
	mk := testMasterKey()
	t.Setenv(crypto.EnvMasterKey, mk)

	tmpDir := t.TempDir()
	cfgPath := tmpDir + "/config.json"

	cfg := &config.Config{
		TelegramBotToken: "123:ABC",
		OwnerID:          123,
		Accounts: map[string]*config.Account{
			"accA": {ID: "accA", PhoneNumber: "111", AuthToken: "init-a", RefreshToken: "init-a-r"},
			"accB": {ID: "accB", PhoneNumber: "222", AuthToken: "init-b", RefreshToken: "init-b-r"},
			"accC": {ID: "accC", PhoneNumber: "333", AuthToken: "init-c", RefreshToken: "init-c-r"},
		},
	}

	accountIDs := []string{"accA", "accB", "accC"}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			aid := accountIDs[idx%3]
			sess := fmt.Sprintf("sess_%d_for_%s", idx, aid)
			refresh := fmt.Sprintf("refresh_%d_for_%s", idx, aid)
			cfg.UpdateTokensForAccount(aid, sess, refresh)
			if err := cfg.Save(cfgPath); err != nil {
				t.Logf("Save error from goroutine %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	// Load and verify token isolation — each account's token must belong to it
	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	for _, aid := range accountIDs {
		acc := loaded.Accounts[aid]
		if acc == nil {
			t.Errorf("Account %s missing after concurrent saves", aid)
			continue
		}
		sess := acc.AuthToken
		refresh := acc.RefreshToken

		// Token must contain the correct account ID
		if !strings.Contains(sess, aid) {
			t.Errorf("Account %s AuthToken = %q — doesn't contain own account ID", aid, sess)
		}
		if !strings.Contains(refresh, aid) {
			t.Errorf("Account %s RefreshToken = %q — doesn't contain own account ID", aid, refresh)
		}

		// Token must NOT contain another account's ID (no clobbering)
		for _, otherID := range accountIDs {
			if otherID != aid {
				if strings.Contains(sess, otherID) {
					t.Errorf("Account %s AuthToken contains other account %q: %q", aid, otherID, sess)
				}
				if strings.Contains(refresh, otherID) {
					t.Errorf("Account %s RefreshToken contains other account %q: %q", aid, otherID, refresh)
				}
			}
		}
	}
}

func TestCloneForABDMWriteback(t *testing.T) {
	mk := testMasterKey()
	t.Setenv(crypto.EnvMasterKey, mk)

	tmpDir := t.TempDir()
	cfgPath := tmpDir + "/config.json"

	acc := &config.Account{
		ID:           "acc1",
		PhoneNumber:  "9471392919",
		AuthToken:    "original-sess",
		RefreshToken: "original-refresh",
	}

	global := &config.Config{
		TelegramBotToken: "123:ABC",
		OwnerID:          123,
		HipID:            "HIP123",
		Timezone:         "Asia/Kolkata",
		Accounts:         map[string]*config.Account{"acc1": acc},
		SavePath:         cfgPath,
	}

	// Initial save so file exists for writeback
	if err := global.Save(cfgPath); err != nil {
		t.Fatalf("Initial save failed: %v", err)
	}

	// Clone for ABDM
	cloned := acc.CloneForABDM(global)

	newSess := "new-session-token"
	newRefresh := "new-refresh-token"
	cloned.UpdateTokens(newSess, newRefresh)

	// Verify original account tokens updated via writeback closure
	if acc.AuthToken != newSess {
		t.Errorf("Account AuthToken not updated by writeback: got %q, want %q", acc.AuthToken, newSess)
	}
	if acc.RefreshToken != newRefresh {
		t.Errorf("Account RefreshToken not updated by writeback: got %q, want %q", acc.RefreshToken, newRefresh)
	}

	// Verify cloned config's AuthToken/RefreshToken updated
	if cloned.AuthToken != newSess {
		t.Errorf("Cloned AuthToken = %q, want %q", cloned.AuthToken, newSess)
	}
	if cloned.RefreshToken != newRefresh {
		t.Errorf("Cloned RefreshToken = %q, want %q", cloned.RefreshToken, newRefresh)
	}

	// Verify persisted to disk via writeback's global.Save
	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if loaded.Accounts["acc1"].AuthToken != newSess {
		t.Errorf("Persisted AuthToken = %q, want %q", loaded.Accounts["acc1"].AuthToken, newSess)
	}
	if loaded.Accounts["acc1"].RefreshToken != newRefresh {
		t.Errorf("Persisted RefreshToken = %q, want %q", loaded.Accounts["acc1"].RefreshToken, newRefresh)
	}
}

func TestAccountEncryptDecrypt(t *testing.T) {
	mk := testMasterKey()

	tmpDir := t.TempDir()
	cfgPath := tmpDir + "/config.json"

	origAuth := "account-auth-token-789"
	origRefresh := "account-refresh-token-012"

	acc := &config.Account{
		ID:           "acc1",
		PhoneNumber:  "9471392919",
		AuthToken:    origAuth,
		RefreshToken: origRefresh,
	}

	cfg := &config.Config{
		TelegramBotToken: "123:ABC",
		OwnerID:          123,
		Accounts:         map[string]*config.Account{"acc1": acc},
	}

	// Save WITHOUT encryption — tokens stored as plaintext
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("Save without encryption failed: %v", err)
	}
	rawPlain, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(rawPlain), origAuth) {
		t.Error("Unencrypted save should contain plaintext token")
	}

	// Enable encryption and save — encryptTokens/decryptTokens called internally by Save
	t.Setenv(crypto.EnvMasterKey, mk)
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("Save with encryption failed: %v", err)
	}

	// In-memory tokens restored (Save calls decryptTokens in defer)
	if acc.AuthToken != origAuth {
		t.Errorf("AuthToken not restored after encrypted save: got %q, want %q", acc.AuthToken, origAuth)
	}
	if acc.RefreshToken != origRefresh {
		t.Errorf("RefreshToken not restored after encrypted save: got %q, want %q", acc.RefreshToken, origRefresh)
	}

	// Raw file must NOT contain plaintext tokens
	rawEnc, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("Failed to read raw file: %v", err)
	}
	if strings.Contains(string(rawEnc), origAuth) {
		t.Error("Raw file contains plaintext AuthToken after encrypted save")
	}
	if strings.Contains(string(rawEnc), origRefresh) {
		t.Error("Raw file contains plaintext RefreshToken after encrypted save")
	}

	// LoadConfig decrypts — tokens should be restored
	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if loaded.Accounts["acc1"].AuthToken != origAuth {
		t.Errorf("Decrypted AuthToken = %q, want %q", loaded.Accounts["acc1"].AuthToken, origAuth)
	}
	if loaded.Accounts["acc1"].RefreshToken != origRefresh {
		t.Errorf("Decrypted RefreshToken = %q, want %q", loaded.Accounts["acc1"].RefreshToken, origRefresh)
	}
}

func TestPerAccountTokenIsolation(t *testing.T) {
	accA := &config.Account{
		ID:           "accA",
		PhoneNumber:  "111",
		AuthToken:    "initial-a",
		RefreshToken: "initial-a-refresh",
	}
	accB := &config.Account{
		ID:           "accB",
		PhoneNumber:  "222",
		AuthToken:    "initial-b",
		RefreshToken: "initial-b-refresh",
	}

	cfg := &config.Config{
		TelegramBotToken: "123:ABC",
		OwnerID:          123,
		Accounts: map[string]*config.Account{
			"accA": accA,
			"accB": accB,
		},
	}

	// Update account A
	cfg.UpdateTokensForAccount("accA", "tokenA-sess", "tokenA-refresh")

	// Verify A updated, B unchanged
	if accA.AuthToken != "tokenA-sess" {
		t.Errorf("accA AuthToken = %q, want %q", accA.AuthToken, "tokenA-sess")
	}
	if accA.RefreshToken != "tokenA-refresh" {
		t.Errorf("accA RefreshToken = %q, want %q", accA.RefreshToken, "tokenA-refresh")
	}
	if accB.AuthToken != "initial-b" {
		t.Errorf("accB AuthToken clobbered: got %q, want %q", accB.AuthToken, "initial-b")
	}
	if accB.RefreshToken != "initial-b-refresh" {
		t.Errorf("accB RefreshToken clobbered: got %q, want %q", accB.RefreshToken, "initial-b-refresh")
	}

	// Update account B
	cfg.UpdateTokensForAccount("accB", "tokenB-sess", "tokenB-refresh")

	// Verify B updated, A still unchanged
	if accB.AuthToken != "tokenB-sess" {
		t.Errorf("accB AuthToken = %q, want %q", accB.AuthToken, "tokenB-sess")
	}
	if accB.RefreshToken != "tokenB-refresh" {
		t.Errorf("accB RefreshToken = %q, want %q", accB.RefreshToken, "tokenB-refresh")
	}
	if accA.AuthToken != "tokenA-sess" {
		t.Errorf("accA AuthToken changed by B update: got %q, want %q", accA.AuthToken, "tokenA-sess")
	}
	if accA.RefreshToken != "tokenA-refresh" {
		t.Errorf("accA RefreshToken changed by B update: got %q, want %q", accA.RefreshToken, "tokenA-refresh")
	}
}

// ============================================================
// SECTION 14: LOGIN STATE ISOLATION
// ============================================================

func TestLoginStateIsolation(t *testing.T) {
	// loginState is unexported in pkg/bot, so we simulate the structure.
	type loginState struct {
		phase string
		otpCh chan string
		phrCh chan string
	}

	states := make(map[string]*loginState)

	// Create 3 concurrent "login" states with different account IDs
	accountIDs := []string{"acc-alpha", "acc-beta", "acc-gamma"}
	for _, id := range accountIDs {
		states[id] = &loginState{
			phase: "awaiting_otp",
			otpCh: make(chan string, 1),
			phrCh: make(chan string, 1),
		}
	}

	// Send OTP to account A's channel only
	testOTP := "887766"
	states["acc-alpha"].otpCh <- testOTP

	// Verify only A receives it
	select {
	case otp := <-states["acc-alpha"].otpCh:
		if otp != testOTP {
			t.Errorf("acc-alpha received OTP %q, want %q", otp, testOTP)
		}
	default:
		t.Fatal("acc-alpha should have received OTP but channel was empty")
	}

	// Verify B and C don't receive anything (non-blocking)
	if tryRecv(states["acc-beta"].otpCh) {
		t.Error("acc-beta should NOT have received OTP")
	}
	if tryRecv(states["acc-gamma"].otpCh) {
		t.Error("acc-gamma should NOT have received OTP")
	}

	// Send phrase to account C's channel
	testPhrase := "my-secret-phrase"
	states["acc-gamma"].phrCh <- testPhrase

	select {
	case phr := <-states["acc-gamma"].phrCh:
		if phr != testPhrase {
			t.Errorf("acc-gamma received phrase %q, want %q", phr, testPhrase)
		}
	default:
		t.Fatal("acc-gamma should have received phrase")
	}

	// Verify A and B don't have phrase
	if tryRecv(states["acc-alpha"].phrCh) {
		t.Error("acc-alpha should NOT have received phrase")
	}
	if tryRecv(states["acc-beta"].phrCh) {
		t.Error("acc-beta should NOT have received phrase")
	}

	// Each state is independent — phases can differ
	states["acc-alpha"].phase = "awaiting_phrase"
	states["acc-beta"].phase = "complete"
	states["acc-gamma"].phase = "awaiting_otp"

	if states["acc-alpha"].phase != "awaiting_phrase" {
		t.Error("acc-alpha phase should be awaiting_phrase")
	}
	if states["acc-beta"].phase != "complete" {
		t.Error("acc-beta phase should be complete")
	}
	if states["acc-gamma"].phase != "awaiting_otp" {
		t.Error("acc-gamma phase should be awaiting_otp")
	}
}

// tryRecv attempts a non-blocking read from a string channel.
// Returns true if a value was available.
func tryRecv(ch chan string) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// ============================================================
// SECTION 15: OTP WAITER ROUTING
// ============================================================

func TestOTPWaiterRouting(t *testing.T) {
	// Simulates the otpWaiters map used in the bot service.
	otpWaiters := make(map[string]chan string)

	// Add channels for 3 account IDs
	accountIDs := []string{"phone-1111", "phone-2222", "phone-3333"}
	for _, id := range accountIDs {
		otpWaiters[id] = make(chan string, 1)
	}

	// Send OTP to specific account channel
	targetOTP := "123456"
	otpWaiters["phone-2222"] <- targetOTP

	// Verify only target receives correct OTP
	select {
	case otp := <-otpWaiters["phone-2222"]:
		if otp != targetOTP {
			t.Errorf("phone-2222 received OTP %q, want %q", otp, targetOTP)
		}
	default:
		t.Fatal("phone-2222 channel should have OTP")
	}

	// Verify non-target accounts don't receive anything
	if tryRecv(otpWaiters["phone-1111"]) {
		t.Error("phone-1111 should not have received anything")
	}
	if tryRecv(otpWaiters["phone-3333"]) {
		t.Error("phone-3333 should not have received anything")
	}

	// Send different OTPs to each
	otpWaiters["phone-1111"] <- "111111"
	otpWaiters["phone-3333"] <- "333333"

	// Verify each gets its own OTP
	if otp := <-otpWaiters["phone-1111"]; otp != "111111" {
		t.Errorf("phone-1111 OTP = %q, want 111111", otp)
	}
	if otp := <-otpWaiters["phone-3333"]; otp != "333333" {
		t.Errorf("phone-3333 OTP = %q, want 333333", otp)
	}

	// Test routing with concurrent sends
	var wg sync.WaitGroup
	for i, id := range accountIDs {
		wg.Add(1)
		go func(idx int, accountID string) {
			defer wg.Done()
			otpWaiters[accountID] <- "otp-" + accountID
		}(i, id)
	}
	wg.Wait()

	// Each should have its own OTP
	for _, id := range accountIDs {
		select {
		case otp := <-otpWaiters[id]:
			expected := "otp-" + id
			if otp != expected {
				t.Errorf("concurrent: %s received %q, want %q", id, otp, expected)
			}
		default:
			t.Errorf("concurrent: %s channel empty after send", id)
		}
	}
}

// ============================================================
// SECTION 16: ACCOUNT SELECTION CONCURRENCY
// ============================================================

func TestAccountSelectionConcurrent(t *testing.T) {
	acc := config.NewAccount("9900112233")

	const numPatients = 10
	for i := range numPatients {
		acc.Patients = append(acc.Patients, config.Patient{
			OID:  fmt.Sprintf("patient-%d", i),
			FLN:  fmt.Sprintf("Patient %d", i),
			ABHA: fmt.Sprintf("abha-%d@abdm", i),
		})
	}

	// Account.SelectedIDs map is not thread-safe, so serialize goroutine access
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Toggle different patient selections from concurrent goroutines
	wg.Add(numPatients)
	for i := range numPatients {
		go func(idx int) {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			oid := fmt.Sprintf("patient-%d", idx)
			acc.TogglePatientSelection(oid)
		}(i)
	}
	wg.Wait()

	// All 10 should be selected
	selected := acc.GetSelectedPatients()
	if len(selected) != numPatients {
		t.Errorf("after toggling all ON, selected count = %d, want %d", len(selected), numPatients)
	}

	// Toggle half OFF concurrently
	wg.Add(numPatients / 2)
	for i := 0; i < numPatients/2; i++ {
		go func(idx int) {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			oid := fmt.Sprintf("patient-%d", idx)
			acc.TogglePatientSelection(oid)
		}(i)
	}
	wg.Wait()

	// Should have numPatients - numPatients/2 selected
	selected = acc.GetSelectedPatients()
	expected := numPatients - numPatients/2
	if len(selected) != expected {
		t.Errorf("after toggling half OFF, selected count = %d, want %d", len(selected), expected)
	}

	// All goroutines call SelectAllPatients concurrently
	wg.Add(numPatients)
	for range numPatients {
		go func() {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			acc.SelectAllPatients()
		}()
	}
	wg.Wait()

	// All should be selected
	selected = acc.GetSelectedPatients()
	if len(selected) != numPatients {
		t.Errorf("after concurrent SelectAll, selected count = %d, want %d", len(selected), numPatients)
	}

	// Verify SelectedCount matches
	if sc := acc.SelectedCount(); sc != numPatients {
		t.Errorf("SelectedCount = %d, want %d", sc, numPatients)
	}

	// ClearSelections and verify
	acc.ClearSelections()
	if len(acc.GetSelectedPatients()) != 0 {
		t.Error("after ClearSelections, selected should be empty")
	}
	if acc.SelectedCount() != 0 {
		t.Errorf("after ClearSelections, SelectedCount = %d, want 0", acc.SelectedCount())
	}
}
