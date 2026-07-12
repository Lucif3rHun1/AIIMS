package bot

import (
	"context"
	"fmt"
	"log/slog"
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

type outgoingMsg struct {
	chatID int64
	text   string
}

type BotState int

const (
	StateIdle BotState = iota
	StatePhoneInput
	StateLoginOTP
	StateLoginPHR
	StateFetching
	StatePatientSelect
	StateDateInput
	StateDateSelect
	StateRunning
)

func (s BotState) String() string {
	switch s {
	case StateIdle:
		return "Idle"
	case StatePhoneInput:
		return "PhoneInput"
	case StateLoginOTP:
		return "LoginOTP"
	case StateLoginPHR:
		return "LoginPHR"
	case StateFetching:
		return "Fetching"
	case StatePatientSelect:
		return "PatientSelect"
	case StateDateInput:
		return "DateInput"
	case StateDateSelect:
		return "DateSelect"
	case StateRunning:
		return "Running"
	default:
		return "Unknown"
	}
}

type loginState struct {
	phase string
	otpCh chan string
	phrCh chan string
}

type BotService struct {
	bot      *tgbotapi.BotAPI
	config   *config.Config
	notifier *notify.Notifier
	updates  tgbotapi.UpdatesChannel
	mu       sync.RWMutex
	running  atomic.Bool

	registry *runnerRegistry

	editingField string

	loginStates map[string]*loginState
	otpWaiters  map[string]chan string
	otpGate     sync.Mutex // serializes OTP requests so only one account asks at a time (ponytail: single global lock; per-account gates if throughput matters)

	// State machine
	state          BotState
	stateAccountID string
	stateCancelCh  chan struct{}

	waitingForDate  bool
	pendingDateChat int64

	waitingForPhone bool
	phoneInputChat  int64

	sendQueue chan outgoingMsg
	sendWg    sync.WaitGroup

	msgCache   map[int64]int
	msgCacheMu sync.RWMutex

	tokenRefresher *TokenRefresher
}

func NewBotService(cfg *config.Config) (*BotService, error) {
	bot, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, err
	}

	slog.Info("bot authorized", "username", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	var notifier *notify.Notifier
	if cfg.HasBroadcastChannel() {
		notifier = notify.NewNotifier(bot, cfg.BroadcastChatID)
		slog.Info("broadcast channel configured", "chat_id", cfg.BroadcastChatID)
	} else {
		notifier = notify.NewNotifier(bot, 0)
		slog.Warn("no broadcast channel configured")
	}

	svc := &BotService{
		bot:         bot,
		config:      cfg,
		notifier:    notifier,
		updates:     updates,
		registry:    newRunnerRegistry(),
		loginStates: make(map[string]*loginState),
		otpWaiters:  make(map[string]chan string),
		sendQueue:   make(chan outgoingMsg, 100),
		msgCache:    make(map[int64]int),
	}
	svc.tokenRefresher = NewTokenRefresher(cfg)
	svc.sendWg.Add(1)
	go svc.sendWorker()
	return svc, nil
}

func (b *BotService) Start(ctx context.Context, chatID int64) {
	b.running.Store(true)
	b.tokenRefresher.Start()
	b.AutoResume(ctx, chatID)
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
	b.SaveState()
	if b.tokenRefresher != nil {
		b.tokenRefresher.Stop()
	}
	if b.registry != nil {
		b.registry.stopAll()
	}
	close(b.sendQueue)
	b.sendWg.Wait()
}

func (b *BotService) sendWorker() {
	defer b.sendWg.Done()
	for msg := range b.sendQueue {
		tgMsg := tgbotapi.NewMessage(msg.chatID, msg.text)
		tgMsg.ParseMode = tgbotapi.ModeHTML
		sent, err := b.bot.Send(tgMsg)
		if err != nil {
			slog.Error("send message failed", "component", "bot", "error", err)
			continue
		}
		b.msgCacheMu.Lock()
		b.msgCache[msg.chatID] = sent.MessageID
		b.msgCacheMu.Unlock()
	}
}

func (b *BotService) send(chatID int64, text string) {
	b.mu.RLock()
	queue := b.sendQueue
	b.mu.RUnlock()
	if queue == nil {
		return
	}
	select {
	case queue <- outgoingMsg{chatID: chatID, text: text}:
	default:
		slog.Warn("send queue full, dropping message", "component", "bot")
	}
}

// sendHTML is an alias for send — all messages use HTML ParseMode now.
func (b *BotService) sendHTML(chatID int64, text string) {
	b.send(chatID, text)
}

func (b *BotService) editOrSend(chatID int64, text string) {
	b.msgCacheMu.RLock()
	lastMsgID, exists := b.msgCache[chatID]
	b.msgCacheMu.RUnlock()

	if exists {
		edit := tgbotapi.NewEditMessageText(chatID, lastMsgID, text)
		edit.ParseMode = tgbotapi.ModeHTML
		if _, err := b.bot.Send(edit); err != nil {
			b.send(chatID, text)
		}
	} else {
		b.send(chatID, text)
	}
}

func (b *BotService) notifyChannel(ctx context.Context, text string) {
	if !b.notifier.HasChannel() {
		return
	}
	if err := b.notifier.SendHTML(ctx, text); err != nil {
		slog.Error("channel notify failed", "component", "bot", "error", err)
	}
}

func (b *BotService) SetState(state BotState, accountID string) {
	b.mu.Lock()
	if b.stateCancelCh != nil {
		close(b.stateCancelCh)
	}
	b.state = state
	b.stateAccountID = accountID
	b.stateCancelCh = make(chan struct{})
	slog.Info("bot state changed", "component", "bot", "state", state.String(), "account_id", accountID)
	b.mu.Unlock()
	b.SaveState()
}

func (b *BotService) ClearState() {
	b.mu.Lock()
	if b.stateCancelCh != nil {
		select {
		case <-b.stateCancelCh:
		default:
			close(b.stateCancelCh)
		}
	}
	b.state = StateIdle
	b.stateAccountID = ""
	b.stateCancelCh = nil
	b.mu.Unlock()
	b.SaveState()
}

func (b *BotService) StateCancelCh() <-chan struct{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.stateCancelCh
}

func (b *BotService) CurrentState() BotState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.state
}

func (b *BotService) CancelCurrentFlow(chatID int64) {
	b.ClearState()
	b.mu.Lock()
	b.waitingForPhone = false
	b.phoneInputChat = 0
	b.waitingForDate = false
	b.pendingDateChat = 0
	b.mu.Unlock()
	b.SaveState()

	b.send(chatID, MsgCancelled())
	m := tgbotapi.NewMessage(chatID, "Main Menu")
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = GetMainMenu()
	b.bot.Send(m)
}

func (b *BotService) handleUpdate(ctx context.Context, update tgbotapi.Update) {
	if update.CallbackQuery != nil {
		if !b.config.IsOwner(update.CallbackQuery.From.ID) {
			callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "⛔ Unauthorized")
			b.bot.Request(callback)
			return
		}
		callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "")
		b.bot.Request(callback)
		go b.handleCallback(ctx, update.CallbackQuery)
		return
	}

	if update.Message != nil {
		if !b.config.IsOwner(update.Message.From.ID) {
			return
		}

		b.mu.RLock()
		editing := b.editingField
		dateWait := b.waitingForDate
		phoneWait := b.waitingForPhone
		currentState := b.state
		stateAccID := b.stateAccountID
		otpWaiterCount := len(b.otpWaiters)
		b.mu.RUnlock()

		go func() {
			switch {
			case phoneWait || currentState == StatePhoneInput:
				b.handlePhoneInput(update.Message)

			case dateWait || currentState == StateDateInput:
				b.handleDateInput(update.Message)

			case otpWaiterCount > 0:
				b.handleOTPInput(update.Message)

			case currentState == StateLoginOTP:
				b.handleLoginOTPInput(update.Message, stateAccID)

			case currentState == StateLoginPHR:
				b.handleLoginOTPInput(update.Message, stateAccID)

			case editing != "":
				b.handleConfigEdit(ctx, update.Message)

			case update.Message.IsCommand():
				b.handleCommand(ctx, update.Message)
			}
		}()
	}
}

func (b *BotService) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		m := tgbotapi.NewMessage(msg.Chat.ID, b.buildWelcomeMessage())
		m.ParseMode = tgbotapi.ModeHTML
		m.ReplyMarkup = GetMainMenu()
		b.bot.Send(m)

	case "auth":
		chatID := msg.Chat.ID
		chatType := msg.Chat.Type
		if chatType != "group" && chatType != "supergroup" && chatType != "channel" {
			b.send(chatID, MsgAuthMustBeGroup())
			return
		}

		b.config.SetBroadcastChatID(chatID)
		if err := b.config.Save(b.config.SavePath); err != nil {
		b.send(chatID, MsgAuthFailedSave(err.Error()))
			return
		}
		b.notifier.UpdateChatID(chatID)
		b.send(chatID, MsgAuthRegistered(chatID))

		b.notifyChannel(ctx, MsgAuthChannelRegistered())

	case "stop":
		if b.registry.anyRunning() {
			b.registry.stopAll()
			b.send(msg.Chat.ID, MsgAllStopped())
		}

	case "help":
		b.sendHTML(msg.Chat.ID, MsgHelp())
	}
}

func (b *BotService) buildWelcomeMessage() string {
	if len(b.config.Accounts) == 0 {
		return MsgWelcomeNoAccounts()
	}

	acc := b.config.GetActiveAccount()
	if acc == nil {
		return MsgWelcomeNoActive()
	}

	if acc.AuthToken == "" {
		return MsgWelcomeNotLoggedIn(acc.Name)
	}

	selected := acc.SelectedCount()
	total := acc.PatientCount()

	if total == 0 {
		return MsgWelcomeLoggedInNoPatients(acc.Name, acc.PhoneNumber)
	}

	if selected == 0 {
		return MsgWelcomePatientsNoneSelected(acc.Name, acc.PhoneNumber, total)
	}

	if acc.TargetDate == "" {
		return MsgWelcomeReadyNoDate(acc.Name, selected, total)
	}

	return MsgWelcomeReady(acc.Name, selected, acc.TargetDate)
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
		b.showStatus(chatID)

	case strings.HasPrefix(data, "toggle_"):
		idx, _ := strconv.Atoi(strings.TrimPrefix(data, "toggle_"))
		b.toggleSelection(chatID, idx, cb.Message.MessageID)

	case data == "cmd_done":
		acc := b.config.GetActiveAccount()
		count := 0
		if acc != nil {
			count = acc.SelectedCount()
		}
		b.ClearState()
		b.send(chatID, MsgPatientsSelected(count))
		m := tgbotapi.NewMessage(chatID, "Main Menu")
		m.ParseMode = tgbotapi.ModeHTML
		m.ReplyMarkup = GetMainMenu()
		b.bot.Send(m)

	case data == "cmd_run":
		acc := b.config.GetActiveAccount()
		if acc != nil && acc.SelectedCount() == 0 {
			b.send(chatID, MsgNoPatientsSelected())
			return
		}
		b.showDatePicker(chatID)

	case data == "cmd_select_all":
		b.selectAllPatients(chatID, cb.Message.MessageID)

	case data == "cmd_clear_sel":
		b.clearAllSelections(chatID, cb.Message.MessageID)

	case strings.HasPrefix(data, "date_"):
		b.handleDateSelection(ctx, chatID, data)

	case data == "cmd_stop":
		if b.registry.anyRunning() {
			b.registry.stopAll()
			b.send(chatID, MsgStoppingAll())
		} else {
			b.send(chatID, MsgNoTasksRunning())
		}

	case data == "cmd_start":
		m := tgbotapi.NewMessage(chatID, b.buildWelcomeMessage())
		m.ParseMode = tgbotapi.ModeHTML
		m.ReplyMarkup = GetMainMenu()
		b.bot.Send(m)

	case data == "cmd_cancel":
		b.CancelCurrentFlow(chatID)

	case data == "cmd_help":
		b.sendHTML(chatID, MsgHelp())

	case data == "cmd_settings":
		m := tgbotapi.NewMessage(chatID, MsgSettingsMenu())
		m.ParseMode = tgbotapi.ModeHTML
		m.ReplyMarkup = GetSettingsMenu()
		b.bot.Send(m)

	case data == "cmd_accounts":
		b.showAccountsMenu(chatID)

	case data == "cmd_add_account":
		b.startAddAccount(chatID)

	case data == "cmd_remove_account":
		b.showRemoveAccountMenu(chatID)

	case strings.HasPrefix(data, "switch_"):
		accID := strings.TrimPrefix(data, "switch_")
		b.switchAccount(chatID, accID)

	case strings.HasPrefix(data, "removeacc_"):
		accID := strings.TrimPrefix(data, "removeacc_")
		b.removeAccount(chatID, accID)

	case strings.HasPrefix(data, "edit_"):
		field := strings.TrimPrefix(data, "edit_")
		b.showFieldEditor(chatID, field)

	case strings.HasPrefix(data, "start_edit_"):
		field := strings.TrimPrefix(data, "start_edit_")
		b.mu.Lock()
		b.editingField = field
		b.mu.Unlock()
		b.send(chatID, MsgEnterNewValue(field))
	}
}

func (b *BotService) showAccountsMenu(chatID int64) {
	accounts := b.config.AccountList()
	if len(accounts) == 0 {
		b.send(chatID, MsgNoAccounts())
		return
	}
	m := tgbotapi.NewMessage(chatID, MsgAccountsMenu())
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = GetAccountsMenu(accounts, b.config.ActiveAccountID)
	b.bot.Send(m)
}

func (b *BotService) startAddAccount(chatID int64) {
	b.mu.Lock()
	b.waitingForPhone = true
	b.phoneInputChat = chatID
	b.mu.Unlock()
	b.SetState(StatePhoneInput, "")
	m := tgbotapi.NewMessage(chatID, MsgEnterPhone())
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = GetCancelKeyboard()
	b.bot.Send(m)
}

func (b *BotService) handlePhoneInput(msg *tgbotapi.Message) {
	b.mu.Lock()
	b.waitingForPhone = false
	chatID := b.phoneInputChat
	b.phoneInputChat = 0
	b.mu.Unlock()

	if chatID == 0 {
		chatID = msg.Chat.ID
	}

	phone := strings.TrimSpace(msg.Text)
	if phone == "" || len(phone) < 10 {
		b.send(chatID, MsgInvalidPhone())
		b.mu.Lock()
		b.waitingForPhone = true
		b.phoneInputChat = chatID
		b.mu.Unlock()
		return
	}

	acc := config.NewAccount(phone)
	b.config.AddAccount(acc)
	b.config.SetActiveAccount(phone)
	if err := b.config.Save(b.config.SavePath); err != nil {
		b.send(chatID, MsgSaveFailed(err.Error()))
		b.ClearState()
		return
	}

	b.SetState(StateLoginOTP, phone)
	b.send(chatID, MsgAccountAdded(phone))
	go func() {
		if b.triggerLogin(context.Background(), chatID, phone) {
			b.ClearState()
			b.send(chatID, MsgLoginCompleteFetchPatients())
			b.fetchPatients(context.Background(), chatID)
		} else {
			b.ClearState()
		}
	}()
}

func (b *BotService) switchAccount(chatID int64, accID string) {
	if !b.config.SetActiveAccount(accID) {
		b.send(chatID, MsgAccountNotFound())
		return
	}
	if err := b.config.Save(b.config.SavePath); err != nil {
		b.send(chatID, MsgSaveFailed(err.Error()))
		return
	}

	m := tgbotapi.NewMessage(chatID, b.buildWelcomeMessage())
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = GetMainMenu()
	b.bot.Send(m)
}

func (b *BotService) showRemoveAccountMenu(chatID int64) {
	accounts := b.config.AccountList()
	if len(accounts) == 0 {
		b.send(chatID, MsgNoAccountsToRemove())
		return
	}
	if len(accounts) == 1 {
		b.send(chatID, MsgCannotRemoveOnlyAccount())
		return
	}
	m := tgbotapi.NewMessage(chatID, MsgRemoveAccountMenu())
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = GetRemoveAccountMenu(accounts, b.config.ActiveAccountID)
	b.bot.Send(m)
}

func (b *BotService) removeAccount(chatID int64, accID string) {
	if !b.config.RemoveAccount(accID) {
		b.send(chatID, MsgAccountNotFound())
		return
	}
	if err := b.config.Save(b.config.SavePath); err != nil {
		b.send(chatID, MsgSaveFailed(err.Error()))
		return
	}
	b.send(chatID, MsgAccountRemoved())
	b.showAccountsMenu(chatID)
}

func (b *BotService) showStatus(chatID int64) {
	accounts := b.config.AccountList()
	if len(accounts) == 0 {
		b.send(chatID, MsgNoAccountsStatus())
		return
	}

	var lines []string
	lines = append(lines, MsgStatusHeader())

	for _, acc := range accounts {
		selected := acc.SelectedCount()
		total := acc.PatientCount()
		status := "⏹️ Idle"
		if r := b.registry.get(acc.ID); r != nil && r.running.Load() {
			status = "▶️ Running"
		}
		lines = append(lines, MsgStatusAccount(
			acc.Name, status, acc.PhoneNumber, b.config.HipID, acc.TargetDate, selected, total,
		))
	}

	b.sendHTML(chatID, strings.Join(lines, "\n\n"))
}

func (b *BotService) showFieldEditor(chatID int64, field string) {
	var currentVal string
	switch field {
	case "hip_id":
		currentVal = b.config.HipID
	case "bot_token":
		currentVal = b.config.TelegramBotToken
	case "owner_id":
		currentVal = fmt.Sprintf("%d", b.config.OwnerID)
	case "broadcast_chat_id":
		currentVal = fmt.Sprintf("%d", b.config.BroadcastChatID)
	}

	if len(currentVal) > 20 {
		currentVal = currentVal[:20] + "..."
	}

	msg := tgbotapi.NewMessage(chatID, MsgFieldEditor(field, currentVal))
	msg.ParseMode = tgbotapi.ModeHTML
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
	case "hip_id":
		b.config.HipID = newVal
	case "bot_token":
		b.config.TelegramBotToken = newVal
	case "owner_id":
		if id, err := strconv.ParseInt(newVal, 10, 64); err == nil {
			b.config.OwnerID = id
		} else {
			b.send(msg.Chat.ID, MsgInvalidOwnerID())
			return
		}
	case "broadcast_chat_id":
		if id, err := strconv.ParseInt(newVal, 10, 64); err == nil {
			b.config.BroadcastChatID = id
		} else {
			b.send(msg.Chat.ID, MsgInvalidBroadcastID())
			return
		}
	}

	if err := b.config.Save(b.config.SavePath); err != nil {
		b.send(msg.Chat.ID, MsgConfigSaveFailed(err.Error()))
	} else {
		b.send(msg.Chat.ID, MsgConfigSaved())
	}

	m := tgbotapi.NewMessage(msg.Chat.ID, MsgSettingsMenu())
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = GetSettingsMenu()
	b.bot.Send(m)
}

func (b *BotService) fetchPatients(ctx context.Context, chatID int64) {
	acc := b.config.GetActiveAccount()
	if acc == nil {
		b.send(chatID, MsgNoActiveAccount())
		return
	}

	b.SetState(StateFetching, acc.ID)
	b.send(chatID, MsgFetchingPatients())

	slog.Info("FETCH_DEBUG: spawning goroutine", "account_id", acc.ID)
	go func() {
		slog.Info("FETCH_DEBUG: goroutine entered", "account_id", acc.ID)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("fetch patients panic recovered", "component", "bot", "account_id", acc.ID, "panic", r)
				b.send(chatID, MsgFetchInternalError())
			}
			b.ClearState()
		}()

		fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		slog.Info("fetch patients: context created", "component", "bot", "account_id", acc.ID)

		cfg := acc.CloneForABDM(b.config)
		slog.Info("fetch patients: config cloned", "component", "bot", "account_id", acc.ID)

		manager := abdm.NewABDMManager(cfg, fetchCtx)
		slog.Info("fetch patients: manager created", "component", "bot", "account_id", acc.ID)

		slog.Info("fetch patients: initializing system", "component", "bot", "account_id", acc.ID)
		if err := manager.InitializeSystem(); err != nil {
			slog.Warn("fetch patients: token refresh failed, attempting login", "component", "bot", "account_id", acc.ID, "error", err)
			b.send(chatID, MsgSessionExpired())

			loginCtx, loginCancel := context.WithTimeout(ctx, 8*time.Minute)
			defer loginCancel()

			if !b.triggerLogin(loginCtx, chatID, acc.ID) {
				slog.Warn("fetch patients: login failed or cancelled", "component", "bot", "account_id", acc.ID)
				b.send(chatID, MsgLoginFailedManual())
				return
			}

			cfg = acc.CloneForABDM(b.config)
			manager = abdm.NewABDMManager(cfg, fetchCtx)
			if err := manager.InitializeSystem(); err != nil {
				slog.Error("fetch patients: init failed after login", "component", "bot", "account_id", acc.ID, "error", err)
				b.send(chatID, MsgSessionInvalidAfterLogin())
				return
			}
		}

		slog.Info("fetch patients: fetching from API", "component", "bot", "account_id", acc.ID)
		patients, err := manager.FetchPatients()
		if err != nil {
			slog.Error("fetch patients: API call failed", "component", "bot", "account_id", acc.ID, "error", err)
			b.send(chatID, MsgFetchFailed(err.Error()))
			return
		}

		slog.Info("fetch patients: success", "component", "bot", "account_id", acc.ID, "count", len(patients))
		acc.Patients = toConfigPatients(patients)
		// Preserve existing selections for patients that still exist after fetch
		b.preserveSelections(acc)
		if err := b.config.Save(b.config.SavePath); err != nil {
			slog.Error("fetch patients: save failed", "component", "bot", "account_id", acc.ID, "error", err)
			b.send(chatID, MsgFetchSaveFailed(err.Error()))
		}

		b.SetState(StatePatientSelect, acc.ID)
		selectedCount := acc.SelectedCount()
		// Send text + keyboard as ONE message so order is guaranteed
		m := tgbotapi.NewMessage(chatID, MsgPatientsFound(acc.PhoneNumber, len(patients), selectedCount, len(patients)))
		m.ParseMode = tgbotapi.ModeHTML
		m.ReplyMarkup = GetPatientSelectionKeyboard(acc.Patients, acc.SelectedIDs)
		if _, err := b.bot.Send(m); err != nil {
			slog.Error("fetch patients: failed to send keyboard", "component", "bot", "account_id", acc.ID, "error", err)
			b.send(chatID, MsgPatientKeyboardFailed())
		}
	}()
}

func (b *BotService) preserveSelections(acc *config.Account) {
	existingOIDs := make(map[string]bool, len(acc.Patients))
	for _, p := range acc.Patients {
		existingOIDs[p.OID] = true
	}
	for oid := range acc.SelectedIDs {
		if !existingOIDs[oid] {
			delete(acc.SelectedIDs, oid)
		}
	}
}

func (b *BotService) triggerLogin(ctx context.Context, chatID int64, accountID string) bool {
	b.mu.Lock()
	if _, exists := b.loginStates[accountID]; exists {
		b.mu.Unlock()
		b.send(chatID, MsgLoginAlreadyInProgress())
		return false
	}
	b.loginStates[accountID] = &loginState{
		phase: "otp",
		otpCh: make(chan string, 1),
		phrCh: make(chan string, 1),
	}
	b.mu.Unlock()
	b.SaveState()

	cleanup := func() {
		b.mu.Lock()
		delete(b.loginStates, accountID)
		b.mu.Unlock()
		b.SaveState()
	}

	resultCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("login goroutine panicked, recovered", "component", "bot", "account_id", accountID, "panic", r)
				cleanup()
				select {
				case resultCh <- fmt.Errorf("internal error: %v", r):
				default:
				}
			}
		}()
		err := b.performLogin(ctx, chatID, accountID)
		cleanup()
		select {
		case resultCh <- err:
		default:
		}
	}()

	select {
	case err := <-resultCh:
		if err != nil {
			b.send(chatID, MsgLoginFailed(err.Error()))
			return false
		}
		return true
	case <-time.After(7 * time.Minute):
		cleanup()
		b.send(chatID, MsgLoginTimedOut())
		return false
	case <-b.StateCancelCh():
		cleanup()
		b.send(chatID, MsgLoginCancelled())
		return false
	}
}

func (b *BotService) toggleSelection(chatID int64, idx int, messageID int) {
	acc := b.config.GetActiveAccount()
	if acc == nil || idx < 0 || idx >= len(acc.Patients) {
		return
	}

	acc.TogglePatientSelection(acc.Patients[idx].OID)
	if err := b.config.Save(b.config.SavePath); err != nil {
		slog.Error("save patient selection failed", "component", "bot", "error", err)
	}

	patients := make([]config.Patient, len(acc.Patients))
	copy(patients, acc.Patients)
	selected := make(map[string]bool, len(acc.SelectedIDs))
	for k, v := range acc.SelectedIDs {
		selected[k] = v
	}

	markup := GetPatientSelectionKeyboard(patients, selected)
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, markup)
	b.bot.Send(edit)
}

func (b *BotService) selectAllPatients(chatID int64, messageID int) {
	acc := b.config.GetActiveAccount()
	if acc == nil {
		return
	}

	acc.SelectAllPatients()
	if err := b.config.Save(b.config.SavePath); err != nil {
		slog.Error("save patient selections failed", "component", "bot", "error", err)
	}

	patients := make([]config.Patient, len(acc.Patients))
	copy(patients, acc.Patients)
	selected := make(map[string]bool, len(acc.SelectedIDs))
	for k, v := range acc.SelectedIDs {
		selected[k] = v
	}

	markup := GetPatientSelectionKeyboard(patients, selected)
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, markup)
	b.bot.Send(edit)
}

func (b *BotService) clearAllSelections(chatID int64, messageID int) {
	acc := b.config.GetActiveAccount()
	if acc == nil {
		return
	}

	acc.ClearSelections()
	if err := b.config.Save(b.config.SavePath); err != nil {
		slog.Error("save cleared selections failed", "component", "bot", "error", err)
	}

	patients := make([]config.Patient, len(acc.Patients))
	copy(patients, acc.Patients)
	selected := make(map[string]bool)

	markup := GetPatientSelectionKeyboard(patients, selected)
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, markup)
	b.bot.Send(edit)
}

func (b *BotService) makeGetOTPCallback(chatID int64, accountID string) func(string) (string, error) {
	return func(healthID string) (string, error) {
		// Serialize OTP collection: only one account may request + wait for its
		// OTP at a time. Otherwise all accounts fire MsgOTPRequired at once and
		// the multi-OTP format is required, which fails when one phone maps to
		// several ABHA accounts.
		b.otpGate.Lock()
		defer b.otpGate.Unlock()

		otpCh := make(chan string, 1)

		b.mu.Lock()
		b.otpWaiters[accountID] = otpCh
		b.mu.Unlock()

		defer func() {
			b.mu.Lock()
			delete(b.otpWaiters, accountID)
			b.mu.Unlock()
		}()

		b.send(chatID, MsgOTPRequired(accountID, healthID))

		select {
		case otp := <-otpCh:
			return otp, nil
		case <-time.After(5 * time.Minute):
			return "", fmt.Errorf("OTP timeout")
		case <-b.StateCancelCh():
			return "", fmt.Errorf("operation cancelled")
		}
	}
}

func (b *BotService) runAllAccounts(chatID int64, targetTime time.Time) {
	accounts := b.config.AccountList()
	if len(accounts) == 0 {
		b.send(chatID, MsgNoAccountsConfigured())
		return
	}

	var eligible []*config.Account
	for _, acc := range accounts {
		if acc.SelectedCount() > 0 {
			eligible = append(eligible, acc)
		}
	}
	if len(eligible) == 0 {
		b.send(chatID, MsgNoPatientsSelectedAny())
		return
	}

	loc, _ := time.LoadLocation("Asia/Kolkata")
	hour, min, sec := 6, 0, 0
	if targetTime.Hour() != 0 || targetTime.Minute() != 0 {
		hour, min, sec = targetTime.Hour(), targetTime.Minute(), targetTime.Second()
	}
	target := time.Date(
		targetTime.Year(), targetTime.Month(), targetTime.Day(),
		hour, min, sec, 0, loc,
	)

	b.send(chatID, MsgExecutionStart(len(eligible), target.Format("15:04"), target.Format("02 Jan 2006")))

	for _, acc := range eligible {
		go b.runAccount(chatID, acc, target)
	}
}

func (b *BotService) runAccount(chatID int64, acc *config.Account, target time.Time) {
	runner := newAccountRunner(acc.ID, acc.Name, acc.GetSelectedPatients())
	b.registry.add(runner)
	defer b.registry.remove(acc.ID)

	defer func() {
		if r := recover(); r != nil {
			slog.Error("runner panicked, recovered", "component", "bot", "account", acc.Name, "panic", r)
			b.send(chatID, MsgRunnerPanic(acc.Name, r))
		}
	}()

	cfg := acc.CloneForABDM(b.config)
	err := runner.start(context.Background(), cfg, target,
		func(msg string) { b.send(chatID, MsgRunnerProgress(acc.Name, msg)) },
		b.makeGetOTPCallback(chatID, acc.ID),
		b.buildStatusHandlerForAccount(context.Background(), chatID, acc.Name),
	)

	if err != nil {
		b.send(chatID, MsgTaskFailed(acc.Name, err))
	} else {
		b.send(chatID, MsgTaskCompleted(acc.Name))
	}
}

func (b *BotService) handleOTPInput(msg *tgbotapi.Message) {
	text := strings.TrimSpace(msg.Text)
	if strings.HasPrefix(text, "/") {
		return
	}

	b.mu.RLock()
	waiterCount := len(b.otpWaiters)
	b.mu.RUnlock()

	if waiterCount == 0 {
		return
	}

	if waiterCount == 1 {
		b.mu.RLock()
		var otpCh chan string
		for _, ch := range b.otpWaiters {
			otpCh = ch
			break
		}
		b.mu.RUnlock()

		if otpCh == nil {
			return
		}
		select {
		case otpCh <- text:
		default:
					b.send(msg.Chat.ID, MsgOTPChannelBusy())
		}
		return
	}

	parts := strings.Fields(text)
	if len(parts) >= 2 {
		otp := parts[0]
		accHint := parts[1]

		b.mu.RLock()
		for accID, ch := range b.otpWaiters {
			if strings.Contains(accID, accHint) || accHint == accID {
				b.mu.RUnlock()
				select {
				case ch <- otp:
				default:
			b.send(msg.Chat.ID, MsgOTPChannelBusy())
				}
				return
			}
		}
		b.mu.RUnlock()
	}

	var accountIDs []string
	b.mu.RLock()
	for accID := range b.otpWaiters {
		accountIDs = append(accountIDs, accID)
	}
	b.mu.RUnlock()

	b.sendHTML(msg.Chat.ID, MsgMultiOTPNeeded(accountIDs, accountIDs[0]))
}

func (b *BotService) performLogin(ctx context.Context, chatID int64, accountID string) error {
	acc := b.config.Accounts[accountID]
	if acc == nil {
		return fmt.Errorf("account not found")
	}

	phone := acc.PhoneNumber
	if phone == "" {
		return fmt.Errorf("phone_number not configured")
	}

	login := abdm.NewABDMLogin()

	slog.Info("initiating login", "component", "bot", "phone", phone)
	b.send(chatID, MsgInitiatingLogin(phone))
	initResp, err := login.InitLogin(ctx, phone)
	if err != nil {
		slog.Error("login init failed", "component", "bot", "phone", phone, "error", err)
		return fmt.Errorf("login init failed: %v", err)
	}
	b.send(chatID, MsgOTPInput(initResp.Hint))

	b.mu.Lock()
	if ls, ok := b.loginStates[accountID]; ok {
		ls.phase = "otp"
	}
	b.mu.Unlock()

	b.SetState(StateLoginOTP, accountID)
	b.mu.RLock()
	var otpCh chan string
	if ls, ok := b.loginStates[accountID]; ok {
		otpCh = ls.otpCh
	}
	b.mu.RUnlock()

	var otp string
	select {
	case otp = <-otpCh:
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("OTP input timeout (5 min)")
	case <-ctx.Done():
		return ctx.Err()
	case <-b.StateCancelCh():
		return fmt.Errorf("login cancelled")
	}

	slog.Info("verifying OTP", "component", "bot", "phone", phone)
	verifyResp, err := login.VerifyOTP(ctx, initResp.TxnID, otp)
	if err != nil {
		slog.Error("OTP verification failed", "component", "bot", "phone", phone, "error", err)
		return fmt.Errorf("OTP verification failed: %v", err)
	}

	profiles := verifyResp.ABHAProfiles
	var selectedPHR string

	if len(profiles) == 0 {
		return fmt.Errorf("no ABHA profiles found")
	}

	if len(profiles) == 1 {
		selectedPHR = profiles[0].ABHAAddress
		b.send(chatID, MsgAutoSelectedProfile(profiles[0].Name, selectedPHR))
	} else {
		var lines []string
		for i, p := range profiles {
			verified := "⏳"
			if p.KYCVerified == "VERIFIED" {
				verified = "✅"
			}
			lines = append(lines, MsgProfileLine(i+1, verified, p.Name, p.ABHAAddress))
		}
		b.sendHTML(chatID, MsgMultiProfileSelect(strings.Join(lines, "\n"), len(profiles)))

		b.mu.Lock()
		if ls, ok := b.loginStates[accountID]; ok {
			ls.phase = "phr"
		}
		b.mu.Unlock()

		b.SetState(StateLoginPHR, accountID)
		b.mu.RLock()
		var phrCh chan string
		if ls, ok := b.loginStates[accountID]; ok {
			phrCh = ls.phrCh
		}
		b.mu.RUnlock()

		var phrInput string
		select {
		case phrInput = <-phrCh:
		case <-time.After(2 * time.Minute):
			return fmt.Errorf("profile selection timeout")
		case <-ctx.Done():
			return ctx.Err()
		case <-b.StateCancelCh():
			return fmt.Errorf("login cancelled")
		}

		idx, err := strconv.Atoi(strings.TrimSpace(phrInput))
		if err != nil || idx < 1 || idx > len(profiles) {
			return fmt.Errorf("invalid selection: %q", phrInput)
		}
		selectedPHR = profiles[idx-1].ABHAAddress
		b.send(chatID, MsgSelectedProfile(profiles[idx-1].Name, selectedPHR))
	}

	slog.Info("selecting PHR", "component", "bot", "phone", phone)
	phrResp, err := login.SelectPHR(ctx, initResp.TxnID, selectedPHR)
	if err != nil {
		slog.Error("PHR selection failed", "component", "bot", "phone", phone, "error", err)
		return fmt.Errorf("PHR selection failed: %v", err)
	}

	b.send(chatID, MsgExchangingTokens())
	sess, refresh, err := login.ExchangeMinToken(ctx, phrResp.EKA.MinToken, phrResp.EKA.OID)
	if err != nil {
		slog.Error("token exchange failed", "component", "bot", "phone", phone, "error", err)
		return fmt.Errorf("token exchange failed: %v", err)
	}

	slog.Info("login tokens obtained, saving config", "component", "bot", "phone", phone)
	acc.UpdateTokens(sess, refresh)
	if err := b.config.Save(b.config.SavePath); err != nil {
		b.send(chatID, MsgTokensSaveFailed(err.Error()))
	} else {
		expiry := abdm.ParseJWTExpiry(sess)
		b.send(chatID, MsgLoginSuccess(expiry.Format("02 Jan 2006 15:04:05")))
	}

	return nil
}

func (b *BotService) handleLoginOTPInput(msg *tgbotapi.Message, accountID string) {
	text := strings.TrimSpace(msg.Text)
	if strings.HasPrefix(text, "/") {
		return
	}

	b.mu.RLock()
	ls, ok := b.loginStates[accountID]
	b.mu.RUnlock()

	if !ok || ls == nil {
		return
	}

	if ls.phase == "otp" {
		if _, err := strconv.Atoi(text); err == nil && len(text) == 6 {
			if ls.otpCh != nil {
				select {
				case ls.otpCh <- text:
				default:
					b.send(msg.Chat.ID, MsgLoginOTPChannelBusy())
				}
			}
		}
		return
	}

	if ls.phase == "phr" {
		if _, err := strconv.Atoi(text); err == nil {
			if ls.phrCh != nil {
				select {
				case ls.phrCh <- text:
				default:
					b.send(msg.Chat.ID, MsgSelectionChannelBusy())
				}
			}
		}
	}
}

func (b *BotService) showDatePicker(chatID int64) {
	acc := b.config.GetActiveAccount()
	if acc == nil {
		b.send(chatID, MsgNoActiveAccountDate())
		return
	}
	b.SetState(StateDateSelect, acc.ID)
	b.send(chatID, MsgDateStep())
	m := tgbotapi.NewMessage(chatID, MsgDateSelectTarget())
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = GetDatePickerKeyboard()
	b.bot.Send(m)
}

func (b *BotService) handleDateSelection(ctx context.Context, chatID int64, data string) {
	if data == "date_custom" {
		b.mu.Lock()
		b.waitingForDate = true
		b.pendingDateChat = chatID
		b.mu.Unlock()
		b.SetState(StateDateInput, "")
		b.send(chatID, MsgDateCustomFormat())
		return
	}

	if b.registry.anyRunning() {
		b.send(chatID, MsgTasksAlreadyRunning())
		return
	}

	offsetStr := strings.TrimPrefix(data, "date_")
	offset, err := strconv.Atoi(offsetStr)
	if err != nil {
		b.send(chatID, MsgDateInvalid())
		return
	}

	loc, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(loc)
	targetDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	targetDate = targetDate.AddDate(0, 0, offset)

	b.ClearState()
	b.SaveState()
	b.send(chatID, MsgDateSet(targetDate.Format("02 Jan 2006 (Monday)")))
	go b.runAllAccounts(chatID, targetDate)
}

func (b *BotService) handleDateInput(msg *tgbotapi.Message) {
	text := strings.TrimSpace(msg.Text)
	if strings.HasPrefix(text, "/") {
		return
	}

	b.mu.Lock()
	b.waitingForDate = false
	chatID := b.pendingDateChat
	b.pendingDateChat = 0
	b.mu.Unlock()

	if chatID == 0 {
		chatID = msg.Chat.ID
	}

	parsed, err := time.Parse("02-01-2006", text)
	if err != nil {
		b.send(chatID, MsgDateInvalidFormat())
		b.mu.Lock()
		b.waitingForDate = true
		b.pendingDateChat = chatID
		b.mu.Unlock()
		return
	}

	if b.registry.anyRunning() {
		b.send(chatID, MsgTasksAlreadyRunning())
		return
	}

	loc, _ := time.LoadLocation("Asia/Kolkata")
	targetDate := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, loc)

	b.ClearState()
	b.SaveState()
	b.send(chatID, MsgDateSet(targetDate.Format("02 Jan 2006 (Monday)")))
	go b.runAllAccounts(chatID, targetDate)
}

func (b *BotService) buildStatusHandler(ctx context.Context, chatID int64) abdm.StatusCallback {
	return b.buildStatusHandlerForAccount(ctx, chatID, "")
}

func (b *BotService) buildStatusHandlerForAccount(ctx context.Context, chatID int64, accountName string) abdm.StatusCallback {
	prefix := ""
	if accountName != "" {
		prefix = fmt.Sprintf("[%s] ", accountName)
	}

	send := func(text string) {
		msg := prefix + text
		if b.notifier.HasChannel() {
			b.notifyChannel(ctx, msg)
		} else {
			b.sendHTML(chatID, msg)
		}
	}

	return func(event string, details map[string]interface{}) {
		switch event {
		case "waiting":
			waitSec, _ := details["wait_seconds"].(float64)
			send(MsgWaitingForTarget(waitSec, fmt.Sprintf("%v", details["target"])))
		case "prewarm_started":
			tp, _ := details["total_patients"].(int)
			tminus, _ := details["t_minus"].(string)
			send(MsgPrewarmStarted(tp, tminus))
		case "prewarm_token_refreshed":
			tp, _ := details["total_patients"].(int)
			send(MsgPrewarmTokenRefreshed(tp))
		case "prewarm_progress":
			patient, _ := details["patient"].(string)
			ready, _ := details["ready"].(int)
			total, _ := details["total"].(int)
			send(MsgPrewarmTokenReady(patient, ready, total))
		case "prewarm_complete":
			ready, _ := details["ready"].(int)
			failed, _ := details["failed"].(int)
			total, _ := details["total"].(int)
			send(MsgPrewarmComplete(ready, failed, total))
		case "execution_started":
			tp, _ := details["total_patients"].(int)
			ts, _ := details["target"].(string)
			send(notify.FormatExecutionStart(tp, ts))
		case "validation_progress":
			patient, _ := details["patient"].(string)
			validated, _ := details["validated"].(int)
			total, _ := details["total"].(int)
			send(notify.FormatValidationStatus(patient, validated, total, ""))
		case "validation_failed":
			patient, _ := details["patient"].(string)
			errMsg, _ := details["error"].(string)
			send(notify.FormatValidationStatus(patient, 0, 0, errMsg))
		case "appointment_confirmed":
			patient, _ := details["patient"].(string)
			tokenNum, _ := details["token_number"].(string)
			healthID, _ := details["health_id"].(string)
			hipName, _ := details["hip_name"].(string)
			send(notify.FormatAppointmentSuccess(
				patient, healthID, tokenNum, hipName, "", "Proceed to counter for OPD slip",
			))
		case "patient_failed":
			patient, _ := details["patient"].(string)
			send(notify.FormatBurstResult(patient, false, ""))
		case "execution_complete":
			sc, _ := details["success"].(int32)
			fc, _ := details["failed"].(int32)
			tot, _ := details["total"].(int)
			send(notify.FormatExecutionComplete(sc, fc, tot))
		}
	}
}

func toConfigPatients(patients []abdm.Patient) []config.Patient {
	result := make([]config.Patient, len(patients))
	for i, p := range patients {
		result[i] = config.Patient{
			OID:       p.OID,
			FLN:       p.FLN,
			HealthIDs: p.HealthIDs,
			ABHA:      p.ABHA,
		}
	}
	return result
}

func toABDMPatients(patients []config.Patient) []abdm.Patient {
	result := make([]abdm.Patient, len(patients))
	for i, p := range patients {
		result[i] = abdm.Patient{
			OID:       p.OID,
			FLN:       p.FLN,
			HealthIDs: p.HealthIDs,
			ABHA:      p.ABHA,
		}
	}
	return result
}
