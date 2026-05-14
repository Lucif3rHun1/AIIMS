package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aiims-appointment/pkg/abdm"
	"aiims-appointment/pkg/config"
	"aiims-appointment/pkg/notify"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type BotService struct {
	bot      *tgbotapi.BotAPI
	config   *config.Config
	notifier *notify.Notifier
	manager  *abdm.ABDMManager
	updates  tgbotapi.UpdatesChannel
	mu       sync.RWMutex
	running  atomic.Bool

	patients       []abdm.Patient
	selectedIDs    map[string]bool
	otpForHealthID string
	waitingForOTP  bool
	editingField   string
	otpChannel     chan string
	taskCancel     context.CancelFunc
}

func NewBotService(cfg *config.Config) (*BotService, error) {
	bot, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, err
	}

	log.Printf("Authorized: %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	var notifier *notify.Notifier
	if cfg.HasBroadcastChannel() {
		notifier = notify.NewNotifier(bot, cfg.BroadcastChatID)
		log.Printf("Broadcast channel configured: %d", cfg.BroadcastChatID)
	} else {
		notifier = notify.NewNotifier(bot, 0)
		log.Println("No broadcast channel — use /auth in a channel to register")
	}

	return &BotService{
		bot:         bot,
		config:      cfg,
		notifier:    notifier,
		updates:     updates,
		selectedIDs: make(map[string]bool),
		otpChannel:  make(chan string, 1),
	}, nil
}

func (b *BotService) Start(ctx context.Context) {
	b.running.Store(true)
	for {
		select {
		case <-ctx.Done():
			b.running.Store(false)
			return
		case update := <-b.updates:
			b.handleUpdate(ctx, update)
		}
	}
}

func (b *BotService) Stop() {
	if b.taskCancel != nil {
		b.taskCancel()
	}
	if b.manager != nil {
		b.manager.Close()
	}
}

func (b *BotService) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.bot.Send(msg); err != nil {
		log.Printf("[bot] send error: %v", err)
	}
}

func (b *BotService) sendMarkdown(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeMarkdown
	if _, err := b.bot.Send(msg); err != nil {
		log.Printf("[bot] send markdown error: %v", err)
	}
}

func (b *BotService) notifyChannel(ctx context.Context, text string) {
	if !b.notifier.HasChannel() {
		return
	}
	if err := b.notifier.SendMarkdown(ctx, text); err != nil {
		log.Printf("[bot] channel notify error: %v", err)
	}
}

func (b *BotService) handleUpdate(ctx context.Context, update tgbotapi.Update) {
	if update.CallbackQuery != nil {
		b.handleCallback(ctx, update.CallbackQuery)
		return
	}

	if update.Message != nil {
		b.mu.RLock()
		waiting := b.waitingForOTP
		editing := b.editingField
		b.mu.RUnlock()

		if waiting {
			b.handleOTPInput(update.Message)
		} else if editing != "" {
			b.handleConfigEdit(ctx, update.Message)
		} else if update.Message.IsCommand() {
			b.handleCommand(ctx, update.Message)
		}
	}
}

func (b *BotService) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		m := tgbotapi.NewMessage(msg.Chat.ID, "👋 Welcome! Use the menu below.")
		m.ReplyMarkup = GetMainMenu()
		b.bot.Send(m)

	case "auth":
		if !b.config.IsOwner(msg.From.ID) {
			b.send(msg.Chat.ID, "⛔ Unauthorized. Only the owner can register broadcast channels.")
			return
		}
		chatID := msg.Chat.ID
		chatType := msg.Chat.Type
		if chatType != "group" && chatType != "supergroup" && chatType != "channel" {
			b.send(chatID, "⚠️ /auth must be used in a group or channel.")
			return
		}

		b.config.SetBroadcastChatID(chatID)
		if err := b.config.Save("config.json"); err != nil {
			b.send(chatID, "❌ Failed to save: "+err.Error())
			return
		}
		b.notifier.UpdateChatID(chatID)
		b.send(chatID, fmt.Sprintf("✅ Broadcast channel registered! Chat ID: `%d`", chatID))

		b.notifyChannel(ctx, "📡 *Channel Registered*\n\nAll appointment status updates will be sent here.")

	case "stop":
		if b.taskCancel != nil {
			b.taskCancel()
			b.send(msg.Chat.ID, "🛑 Task cancelled.")
		}
	}
}

func (b *BotService) handleCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	callback := tgbotapi.NewCallback(cb.ID, "")
	b.bot.Request(callback)

	chatID := cb.Message.Chat.ID
	data := cb.Data

	switch {
	case data == "cmd_fetch":
		b.fetchPatients(ctx, chatID)

	case data == "cmd_status":
		var validated, total int
		var sc, fc int32
		if b.manager != nil {
			pool := b.manager.GetValidatedPool()
			validated = len(pool)
			total = len(b.patients)
		}
		b.sendMarkdown(chatID, notify.BuildStatusMessage(validated, total, sc, fc, b.taskCancel != nil))

	case strings.HasPrefix(data, "toggle_"):
		idx, _ := strconv.Atoi(strings.TrimPrefix(data, "toggle_"))
		b.toggleSelection(chatID, idx, cb.Message.MessageID)

	case data == "cmd_done":
		b.mu.RLock()
		count := len(b.selectedIDs)
		b.mu.RUnlock()
		b.send(chatID, fmt.Sprintf("✅ %d Patients selected.", count))
		m := tgbotapi.NewMessage(chatID, "Ready to execute?")
		m.ReplyMarkup = GetMainMenu()
		b.bot.Send(m)

	case data == "cmd_run":
		go b.runTask(chatID)

	case data == "cmd_stop":
		if b.taskCancel != nil {
			b.taskCancel()
			b.send(chatID, "🛑 Stopping task...")
		}

	case data == "cmd_start":
		m := tgbotapi.NewMessage(chatID, "Main Menu")
		m.ReplyMarkup = GetMainMenu()
		b.bot.Send(m)

	case data == "cmd_settings":
		m := tgbotapi.NewMessage(chatID, "⚙️ Settings Menu")
		m.ReplyMarkup = GetSettingsMenu()
		b.bot.Send(m)

	case strings.HasPrefix(data, "edit_"):
		field := strings.TrimPrefix(data, "edit_")
		b.showFieldEditor(chatID, field)

	case strings.HasPrefix(data, "start_edit_"):
		field := strings.TrimPrefix(data, "start_edit_")
		b.mu.Lock()
		b.editingField = field
		b.mu.Unlock()
		b.send(chatID, fmt.Sprintf("✏️ Enter new value for %s:", field))
	}
}

func (b *BotService) showFieldEditor(chatID int64, field string) {
	var currentVal string
	switch field {
	case "auth_token":
		currentVal = b.config.AuthToken
	case "refresh_token":
		currentVal = b.config.RefreshToken
	case "target_date":
		currentVal = b.config.TargetDate
	}

	if len(currentVal) > 20 {
		currentVal = currentVal[:20] + "..."
	}

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🔧 **%s**\nCurrent: `%s`", field, currentVal))
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = GetFieldEditMenu(field)
	b.bot.Send(msg)
}

func (b *BotService) handleConfigEdit(ctx context.Context, msg *tgbotapi.Message) {
	b.mu.Lock()
	field := b.editingField
	b.editingField = ""
	b.mu.Unlock()

	if field == "" {
		return
	}

	newVal := strings.TrimSpace(msg.Text)

	switch field {
	case "auth_token":
		b.config.AuthToken = newVal
	case "refresh_token":
		b.config.RefreshToken = newVal
	case "target_date":
		b.config.TargetDate = newVal
	}

	if err := b.config.Save("config.json"); err != nil {
		b.send(msg.Chat.ID, "❌ Failed to save config: "+err.Error())
	} else {
		b.send(msg.Chat.ID, "✅ Config saved successfully!")
	}

	m := tgbotapi.NewMessage(msg.Chat.ID, "⚙️ Settings Menu")
	m.ReplyMarkup = GetSettingsMenu()
	b.bot.Send(m)
}

func (b *BotService) fetchPatients(ctx context.Context, chatID int64) {
	b.send(chatID, "🔄 Fetching...")

	manager := abdm.NewABDMManager(b.config, ctx)
	if err := manager.InitializeSystem(); err != nil {
		b.send(chatID, "❌ Init Failed: "+err.Error())
		return
	}

	patients, err := manager.FetchPatients()
	if err != nil {
		b.send(chatID, "❌ Fetch Failed: "+err.Error())
		return
	}

	b.mu.Lock()
	b.patients = patients
	b.mu.Unlock()

	m := tgbotapi.NewMessage(chatID, "Select Patients:")
	m.ReplyMarkup = GetPatientSelectionKeyboard(patients, b.getSelectedIDs())
	b.bot.Send(m)
}

func (b *BotService) getSelectedIDs() map[string]bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	ids := make(map[string]bool, len(b.selectedIDs))
	for k, v := range b.selectedIDs {
		ids[k] = v
	}
	return ids
}

func (b *BotService) toggleSelection(chatID int64, idx int, messageID int) {
	b.mu.Lock()
	if idx < len(b.patients) {
		oid := b.patients[idx].OID
		if b.selectedIDs[oid] {
			delete(b.selectedIDs, oid)
		} else {
			b.selectedIDs[oid] = true
		}
	}
	patients := make([]abdm.Patient, len(b.patients))
	copy(patients, b.patients)
	selected := make(map[string]bool, len(b.selectedIDs))
	for k, v := range b.selectedIDs {
		selected[k] = v
	}
	b.mu.Unlock()

	markup := GetPatientSelectionKeyboard(patients, selected)
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, markup)
	b.bot.Send(edit)
}

func (b *BotService) runTask(chatID int64) {
	taskCtx, taskCancel := context.WithCancel(context.Background())
	b.taskCancel = taskCancel
	defer func() {
		b.taskCancel = nil
		taskCancel()
	}()

	b.send(chatID, "🚀 Starting Execution...")

	b.mu.RLock()
	selected := make([]abdm.Patient, 0, len(b.selectedIDs))
	for _, p := range b.patients {
		if b.selectedIDs[p.OID] {
			selected = append(selected, p)
		}
	}
	b.mu.RUnlock()

	if b.manager != nil {
		b.manager.Close()
	}
	manager := abdm.NewABDMManager(b.config, taskCtx)
	b.manager = manager

	manager.SendMessage = func(msg string) {
		b.send(chatID, msg)
	}

	manager.GetOTP = func(healthID string) (string, error) {
		b.mu.Lock()
		b.waitingForOTP = true
		b.otpForHealthID = healthID
		b.mu.Unlock()

		b.send(chatID, fmt.Sprintf("🔐 OTP Required for %s. checking SMS...", healthID))
		b.send(chatID, "Please enter the OTP:")

		select {
		case otp := <-b.otpChannel:
			b.mu.Lock()
			b.waitingForOTP = false
			b.mu.Unlock()
			return otp, nil
		case <-time.After(5 * time.Minute):
			b.mu.Lock()
			b.waitingForOTP = false
			b.mu.Unlock()
			return "", fmt.Errorf("OTP timeout")
		case <-taskCtx.Done():
			b.mu.Lock()
			b.waitingForOTP = false
			b.mu.Unlock()
			return "", fmt.Errorf("task cancelled")
		}
	}

	manager.OnStatus = func(event string, details map[string]interface{}) {
		switch event {
		case "waiting":
			b.notifyChannel(taskCtx, fmt.Sprintf(
				"⏳ *Waiting for Target Time*\n\nWaiting %.0f seconds until %s",
				details["wait_seconds"], details["target"],
			))

		case "execution_started":
			tp, _ := details["total_patients"].(int)
			ts, _ := details["target"].(string)
			b.notifyChannel(taskCtx, notify.FormatExecutionStart(tp, ts))

		case "validation_progress":
			patient, _ := details["patient"].(string)
			validated, _ := details["validated"].(int)
			total, _ := details["total"].(int)
			b.notifyChannel(taskCtx, notify.FormatValidationStatus(patient, validated, total, ""))

		case "validation_failed":
			patient, _ := details["patient"].(string)
			errMsg, _ := details["error"].(string)
			b.notifyChannel(taskCtx, notify.FormatValidationStatus(patient, 0, 0, errMsg))

		case "appointment_confirmed":
			patient, _ := details["patient"].(string)
			tokenNum, _ := details["token_number"].(string)
			healthID, _ := details["health_id"].(string)
			hipName, _ := details["hip_name"].(string)
			b.notifyChannel(taskCtx, notify.FormatAppointmentSuccess(
				patient, healthID, tokenNum, hipName, "", "Proceed to counter for OPD slip",
			))

		case "patient_failed":
			patient, _ := details["patient"].(string)
			b.notifyChannel(taskCtx, notify.FormatBurstResult(patient, false, ""))

		case "execution_complete":
			sc, _ := details["success"].(int32)
			fc, _ := details["failed"].(int32)
			tot, _ := details["total"].(int)
			b.notifyChannel(taskCtx, notify.FormatExecutionComplete(sc, fc, tot))
		}
	}

	if err := manager.InitializeSystem(); err != nil {
		b.send(chatID, "❌ Init Failed: "+err.Error())
		return
	}

	manager.ValidatePatients(selected)
	b.notifyChannel(taskCtx, notify.FormatValidationStatus(
		"", len(manager.GetValidatedPool()), len(selected), "",
	))

	t, _ := time.Parse("2006-01-02", b.config.TargetDate)
	if t.IsZero() {
		t = time.Now()
	}

	if err := manager.ExecuteTask(t); err != nil {
		b.send(chatID, "❌ Task Error: "+err.Error())
	}
}

func (b *BotService) handleOTPInput(msg *tgbotapi.Message) {
	select {
	case b.otpChannel <- msg.Text:
	default:
		b.send(msg.Chat.ID, "⚠️ OTP channel busy, try again.")
	}
}
