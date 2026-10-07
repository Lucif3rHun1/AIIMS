package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aiims-appointment/pkg/abdm"
	"aiims-appointment/pkg/appointments"
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
	phase  string
	otpCh  chan string
	phrCh  chan string
	cancel context.CancelFunc
}

const (
	// otpInputTimeout and phrInputTimeout bound how long we wait on the human.
	// loginFlowTimeout must exceed their sum plus network time, or the outer
	// deadline fires while an inner wait is still legitimately running.
	otpInputTimeout  = 5 * time.Minute
	phrInputTimeout  = 2 * time.Minute
	loginFlowTimeout = 10 * time.Minute
	maxOTPAttempts   = 3

	// fetchNetworkTimeout covers network work ONLY. It must never span an
	// interactive login, which blocks on a human reading an SMS.
	fetchNetworkTimeout = 30 * time.Second
)

// istLoc is resolved once. cmd/bot imports _ "time/tzdata" so the lookup
// cannot fail, and the fixed fallback makes a nil *Location — which panics
// inside time.Date — impossible regardless.
var istLoc = func() *time.Location {
	if l, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		return l
	}
	slog.Error("tz database unavailable, using fixed +05:30", "component", "bot")
	return time.FixedZone("IST", 5*60*60+30*60)
}()

// safego runs fn in a goroutine behind a panic barrier. Without it a panic in
// any handler goroutine takes the whole process down.
func safego(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("goroutine panic recovered", "component", "bot",
					"goroutine", name, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// telegramAPI is the slice of *tgbotapi.BotAPI the service actually uses.
// Depending on the interface instead of the struct is what lets the whole
// message/keyboard flow be driven in tests without a Telegram token.
type telegramAPI interface {
	Send(tgbotapi.Chattable) (tgbotapi.Message, error)
	Request(tgbotapi.Chattable) (*tgbotapi.APIResponse, error)
}

type BotService struct {
	bot      telegramAPI
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

	waitingForDate  bool
	pendingDateChat int64

	waitingForPhone bool
	phoneInputChat  int64

	sendQueue chan outgoingMsg
	sendWg    sync.WaitGroup

	tokenRefresher *TokenRefresher

	appointments *appointments.Store // optional; nil if disabled
}

func NewBotService(cfg *config.Config, apptStore *appointments.Store) (*BotService, error) {
	bot, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, err
	}

	slog.Info("bot authorized", "username", bot.Self.UserName)

	if _, err := bot.Request(tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "start", Description: "Show the main menu"},
		tgbotapi.BotCommand{Command: "cancel", Description: "Cancel whatever is in progress"},
		tgbotapi.BotCommand{Command: "status", Description: "Show account and booking status"},
		tgbotapi.BotCommand{Command: "stop", Description: "Stop all running bookings"},
		tgbotapi.BotCommand{Command: "help", Description: "How to use this bot"},
	)); err != nil {
		slog.Warn("could not register bot commands", "component", "bot", "error", err)
	}

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
		bot:          bot,
		config:       cfg,
		notifier:     notifier,
		updates:      updates,
		registry:     newRunnerRegistry(),
		loginStates:  make(map[string]*loginState),
		otpWaiters:   make(map[string]chan string),
		sendQueue:    make(chan outgoingMsg, 100),
		appointments: apptStore,
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
		case update, ok := <-b.updates:
			if !ok {
				// A closed channel returns instantly forever: without this the
				// loop spins a core at 100% and the bot is silently deaf.
				slog.Error("telegram update channel closed, stopping", "component", "bot")
				b.running.Store(false)
				return
			}
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
		if _, err := b.bot.Send(tgMsg); err != nil {
			slog.Error("send message failed", "component", "bot", "error", err)
		}
	}
}

func (b *BotService) send(chatID int64, text string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("send after shutdown", "component", "bot", "panic", r)
		}
	}()
	b.mu.RLock()
	queue := b.sendQueue
	b.mu.RUnlock()
	select {
	case queue <- outgoingMsg{chatID: chatID, text: text}:
	default:
		slog.Warn("send queue full, dropping message", "component", "bot")
	}
}

// sendWithCancel sends a prompt with a Cancel button attached. Text prompts
// without one strand the user, which is what happened on the OTP screen.
func (b *BotService) sendWithCancel(chatID int64, text string) {
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = GetCancelKeyboard()
	if _, err := b.bot.Send(m); err != nil {
		slog.Error("send prompt failed", "component", "bot", "error", err)
		b.send(chatID, text)
	}
}

// sendHTML is an alias for send — all messages use HTML ParseMode now.
func (b *BotService) sendHTML(chatID int64, text string) {
	b.send(chatID, text)
}

// broadcast delivers booking-run output to the chat that started the run and
// mirrors it to the broadcast channel. Status events used to go to the channel
// *instead of* the chat, so whoever pressed the button saw nothing after
// "Waiting...". The equality check stops a run started inside the broadcast
// group from posting everything twice.
func (b *BotService) broadcast(ctx context.Context, chatID int64, text string) {
	b.sendHTML(chatID, text)
	if b.notifier.HasChannel() && b.config.GetBroadcastChatID() != chatID {
		b.notifyChannel(ctx, text)
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
	b.state = state
	b.stateAccountID = accountID
	slog.Info("bot state changed", "component", "bot", "state", state.String(), "account_key", accountKey(accountID))
	b.mu.Unlock()
	b.SaveState()
}

func (b *BotService) ClearState() {
	b.mu.Lock()
	b.state = StateIdle
	b.stateAccountID = ""
	b.mu.Unlock()
	b.SaveState()
}

func (b *BotService) CurrentState() BotState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.state
}

// resetFlow abandons any half-finished interaction: a pending login, a phone
// or date prompt, a settings edit. Every stuck state must be escapable.
func (b *BotService) resetFlow() {
	b.ClearState()
	b.mu.Lock()
	for _, ls := range b.loginStates {
		if ls.cancel != nil {
			ls.cancel()
		}
	}
	b.waitingForPhone = false
	b.phoneInputChat = 0
	b.waitingForDate = false
	b.pendingDateChat = 0
	b.editingField = ""
	b.mu.Unlock()
	b.SaveState()
}

func (b *BotService) CancelCurrentFlow(chatID int64) {
	b.resetFlow()
	m := tgbotapi.NewMessage(chatID, MsgCancelled())
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = b.mainMenu()
	if _, err := b.bot.Send(m); err != nil {
		slog.Error("send main menu failed", "component", "bot", "error", err)
	}
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
		chatID := int64(0)
		if update.CallbackQuery.Message != nil {
			chatID = update.CallbackQuery.Message.Chat.ID
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("dispatcher panic", "panic", r, "chat_id", chatID)
				}
			}()
			b.handleCallback(ctx, update.CallbackQuery)
		}()
		return
	}

	if update.Message != nil {
		if !b.config.IsOwner(update.Message.From.ID) {
			return
		}

		go func() {
			chatID := update.Message.Chat.ID
			defer func() {
				if r := recover(); r != nil {
					slog.Error("dispatcher panic", "panic", r, "chat_id", chatID)
				}
			}()

			b.mu.Lock()
			phoneWait := b.waitingForPhone
			b.waitingForPhone = false
			phoneInputChat := b.phoneInputChat
			b.phoneInputChat = 0
			editing := b.editingField
			b.editingField = ""
			dateWait := b.waitingForDate
			b.waitingForDate = false
			pendingDateChat := b.pendingDateChat
			b.pendingDateChat = 0
			otpWaiterCount := len(b.otpWaiters)
			loginPending := ""
			for id := range b.loginStates {
				loginPending = id
				break
			}
			b.mu.Unlock()

			switch {
			case update.Message.IsCommand():
				// Commands win outright. The flag reads above already cleared any
				// pending input, which is exactly the escape hatch.
				b.handleCommand(ctx, update.Message)
			case phoneWait:
				b.handlePhoneInput(ctx, update.Message, phoneInputChat)
			case editing != "":
				b.handleConfigEdit(ctx, update.Message, editing)
			case dateWait:
				b.handleDateInput(ctx, update.Message, pendingDateChat)
			case loginPending != "":
				// Keyed off a live login, not b.state, so a stray button tap
				// cannot re-route the OTP the user is about to type.
				b.handleLoginOTPInput(update.Message, loginPending)
			case otpWaiterCount > 0:
				b.handleOTPInput(update.Message)
			default:
				b.send(update.Message.Chat.ID, MsgUnrecognizedInput())
			}
		}()
	}
}

// mainMenu builds the state-aware keyboard. Every call site used to hardcode
// the same fixed grid regardless of what the account could actually do.
func (b *BotService) mainMenu() tgbotapi.InlineKeyboardMarkup {
	return GetMainMenu(b.config.GetActiveAccount(), len(b.config.AccountList()), b.registry.anyRunning())
}

func (b *BotService) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		b.resetFlow()
		m := tgbotapi.NewMessage(msg.Chat.ID, b.buildWelcomeMessage())
		m.ParseMode = tgbotapi.ModeHTML
		m.ReplyMarkup = b.mainMenu()
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
		} else {
			b.send(msg.Chat.ID, MsgNoTasksRunning())
		}

	case "cancel":
		b.CancelCurrentFlow(msg.Chat.ID)

	case "status":
		b.showStatus(msg.Chat.ID)

	case "help":
		b.sendHTML(msg.Chat.ID, MsgHelp())

	default:
		b.send(msg.Chat.ID, MsgUnknownCommand(msg.Command()))
	}
}

func (b *BotService) buildWelcomeMessage() string {
	if len(b.config.AccountList()) == 0 {
		return MsgWelcomeNoAccounts()
	}

	acc := b.config.GetActiveAccount()
	if acc == nil {
		return MsgWelcomeNoActive()
	}

	if auth, _ := acc.Tokens(); auth == "" {
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
		m := tgbotapi.NewMessage(chatID, MsgPatientsSelected(count))
		m.ParseMode = tgbotapi.ModeHTML
		m.ReplyMarkup = b.mainMenu()
		if _, err := b.bot.Send(m); err != nil {
			slog.Error("send main menu failed", "component", "bot", "error", err)
		}

	case data == "cmd_run":
		acc := b.config.GetActiveAccount()
		if acc != nil && acc.SelectedCount() == 0 {
			b.send(chatID, MsgNoPatientsSelected())
			return
		}
		b.showDatePicker(chatID)

	case data == "cmd_login":
		acc := b.config.GetActiveAccount()
		if acc == nil {
			b.send(chatID, MsgNoActiveAccount())
			return
		}
		safego("cmdLogin", func() {
			ok := b.triggerLogin(ctx, chatID, acc.ID)
			b.ClearState()
			if ok {
				b.fetchPatients(ctx, chatID)
			}
		})

	case data == "cmd_select":
		acc := b.config.GetActiveAccount()
		if acc == nil || acc.PatientCount() == 0 {
			b.send(chatID, MsgNoPatientsLoaded())
			return
		}
		b.SetState(StatePatientSelect, acc.ID)
		patients, selected := acc.Snapshot()
		m := tgbotapi.NewMessage(chatID, MsgChoosePatients(acc.SelectedCount(), len(patients)))
		m.ParseMode = tgbotapi.ModeHTML
		m.ReplyMarkup = GetPatientSelectionKeyboard(patients, selected)
		if _, err := b.bot.Send(m); err != nil {
			slog.Error("send patient selection failed", "component", "bot", "error", err)
		}

	case data == "cmd_select_all":
		b.selectAllPatients(chatID, cb.Message.MessageID)

	case data == "cmd_clear_sel":
		b.clearAllSelections(chatID, cb.Message.MessageID)

	case strings.HasPrefix(data, "phr_"):
		idx, err := strconv.Atoi(strings.TrimPrefix(data, "phr_"))
		if err != nil {
			return
		}
		b.mu.RLock()
		var phrCh chan string
		for _, ls := range b.loginStates {
			if ls.phase == "phr" {
				phrCh = ls.phrCh
				break
			}
		}
		b.mu.RUnlock()
		if phrCh == nil {
			b.send(chatID, MsgChooseProfileExpired())
			return
		}
		select {
		case phrCh <- strconv.Itoa(idx + 1): // performLogin parses a 1-based index
		default:
		}

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
		m.ReplyMarkup = b.mainMenu()
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

func (b *BotService) handlePhoneInput(ctx context.Context, msg *tgbotapi.Message, chatID int64) {
	if msg.IsCommand() {
		b.handleCommand(ctx, msg)
		return
	}
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

	if b.config.HasAccount(phone) {
		// Re-adding a known phone used to replace the account wholesale,
		// silently wiping its patients, selections and target date.
		b.config.SetActiveAccount(phone)
		if err := b.config.Save(b.config.SavePath); err != nil {
			slog.Error("save active account failed", "component", "bot", "error", err)
		}
		b.send(chatID, MsgAccountExistsReLogin(phone))
		safego("reLogin", func() {
			ok := b.triggerLogin(ctx, chatID, phone)
			b.ClearState()
			if ok {
				b.fetchPatients(ctx, chatID)
			}
		})
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
	// fetchPatients runs itself right after, so telling the user to tap a
	// fetch button here was both redundant and named a button that is gone.
	safego("addAccountLogin", func() {
		defer b.ClearState()
		if b.triggerLogin(context.Background(), chatID, phone) {
			b.ClearState()
			b.fetchPatients(context.Background(), chatID)
		}
	})
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
	m.ReplyMarkup = b.mainMenu()
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

// editableFields is the server-side allowlist. Removing a button from
// GetSettingsMenu is not enough: handleCallback dispatches on an "edit_"
// prefix, so an old keyboard in chat history can still post any field name.
var editableFields = map[string]bool{
	"hip_id":            true,
	"broadcast_chat_id": true,
}

func (b *BotService) showFieldEditor(chatID int64, field string) {
	if !editableFields[field] {
		b.send(chatID, MsgFieldNotEditable(field))
		return
	}

	var currentVal string
	switch field {
	case "hip_id":
		currentVal = b.config.GetHipID()
	case "broadcast_chat_id":
		currentVal = fmt.Sprintf("%d", b.config.GetBroadcastChatID())
	}

	msg := tgbotapi.NewMessage(chatID, MsgFieldEditor(field, currentVal))
	msg.ParseMode = tgbotapi.ModeHTML
	msg.ReplyMarkup = GetFieldEditMenu(field)
	if _, err := b.bot.Send(msg); err != nil {
		slog.Error("send field editor failed", "component", "bot", "error", err)
	}
}

func (b *BotService) handleConfigEdit(ctx context.Context, msg *tgbotapi.Message, field string) {
	// Without this a mistyped /start became the new config value. Writing a
	// command into bot_token bricked the bot in a way a restart could not fix.
	if msg.IsCommand() {
		b.handleCommand(ctx, msg)
		return
	}
	if !editableFields[field] {
		b.send(msg.Chat.ID, MsgFieldNotEditable(field))
		return
	}

	newVal := strings.TrimSpace(msg.Text)
	if err := b.config.SetField(field, newVal); err != nil {
		b.send(msg.Chat.ID, MsgInvalidFieldValue(field, err.Error()))
		return
	}

	if err := b.config.Save(b.config.SavePath); err != nil {
		b.send(msg.Chat.ID, MsgConfigSaveFailed(err.Error()))
	} else {
		b.send(msg.Chat.ID, MsgConfigSaved())
	}

	// Keep the live notifier in step, or the new value silently does nothing
	// until the next restart.
	if field == "broadcast_chat_id" {
		b.notifier.UpdateChatID(b.config.GetBroadcastChatID())
	}

	m := tgbotapi.NewMessage(msg.Chat.ID, MsgSettingsMenu())
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = GetSettingsMenu()
	if _, err := b.bot.Send(m); err != nil {
		slog.Error("send settings menu failed", "component", "bot", "error", err)
	}
}

func (b *BotService) fetchPatients(ctx context.Context, chatID int64) {
	acc := b.config.GetActiveAccount()
	if acc == nil {
		b.send(chatID, MsgNoActiveAccount())
		return
	}

	b.SetState(StateFetching, acc.ID)
	b.send(chatID, MsgFetchingPatients())

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("fetch patients panic recovered", "component", "bot", "account_key", accountKey(acc.ID), "panic", r)
				b.send(chatID, MsgFetchInternalError())
			}
			b.ClearState()
		}()

		// A FRESH deadline per attempt. The old code opened one 30s context
		// before the login and reused it after, so by the time the user had read
		// the SMS and typed the OTP it had long expired -- a fully successful
		// login still reported "session still invalid".
		initOnce := func() (*abdm.ABDMManager, context.CancelFunc, error) {
			netCtx, cancelNet := context.WithTimeout(ctx, fetchNetworkTimeout)
			manager := abdm.NewABDMManager(acc.CloneForABDM(b.config), netCtx)
			if err := manager.InitializeSystem(); err != nil {
				cancelNet()
				return nil, nil, err
			}
			return manager, cancelNet, nil
		}

		manager, cancelNet, err := initOnce()
		if err != nil {
			slog.Warn("fetch patients: token refresh failed, attempting login", "component", "bot", "account_key", accountKey(acc.ID), "error", err)
			b.send(chatID, MsgSessionExpired())

			if !b.triggerLogin(ctx, chatID, acc.ID) {
				slog.Warn("fetch patients: login failed or cancelled", "component", "bot", "account_key", accountKey(acc.ID))
				b.send(chatID, MsgLoginFailedManual())
				return
			}

			manager, cancelNet, err = initOnce()
			if err != nil {
				slog.Error("fetch patients: init failed after login", "component", "bot", "account_key", accountKey(acc.ID), "error", err)
				b.send(chatID, MsgSessionInvalidAfterLogin())
				return
			}
		}
		defer cancelNet()

		patients, err := manager.FetchPatients()
		if err != nil {
			slog.Error("fetch patients: API call failed", "component", "bot", "account_key", accountKey(acc.ID), "error", err)
			b.send(chatID, MsgFetchFailed(err.Error()))
			return
		}

		slog.Info("fetch patients: success", "component", "bot", "account_key", accountKey(acc.ID), "count", len(patients))
		acc.SetPatients(toConfigPatients(patients))
		if err := b.config.Save(b.config.SavePath); err != nil {
			slog.Error("fetch patients: save failed", "component", "bot", "account_key", accountKey(acc.ID), "error", err)
			b.send(chatID, MsgFetchSaveFailed(err.Error()))
		}

		b.SetState(StatePatientSelect, acc.ID)
		snapPatients, snapSelected := acc.Snapshot()
		// Send text + keyboard as ONE message so order is guaranteed
		m := tgbotapi.NewMessage(chatID, MsgPatientsFound(len(patients)))
		m.ParseMode = tgbotapi.ModeHTML
		m.ReplyMarkup = GetPatientSelectionKeyboard(snapPatients, snapSelected)
		if _, err := b.bot.Send(m); err != nil {
			slog.Error("fetch patients: failed to send keyboard", "component", "bot", "account_key", accountKey(acc.ID), "error", err)
			b.send(chatID, MsgPatientKeyboardFailed())
		}
	}()
}

func (b *BotService) triggerLogin(ctx context.Context, chatID int64, accountID string) bool {
	b.mu.Lock()
	if _, exists := b.loginStates[accountID]; exists {
		b.mu.Unlock()
		b.send(chatID, MsgLoginAlreadyInProgress())
		return false
	}
	loginCtx, cancel := context.WithTimeout(ctx, loginFlowTimeout)
	b.loginStates[accountID] = &loginState{
		phase:  "otp",
		otpCh:  make(chan string, 1),
		phrCh:  make(chan string, 1),
		cancel: cancel,
	}
	b.mu.Unlock()
	b.SaveState()

	defer func() {
		cancel()
		b.mu.Lock()
		delete(b.loginStates, accountID)
		b.mu.Unlock()
		b.SaveState()
	}()

	// Inline, not in a goroutine behind an outer timer. performLogin honours
	// loginCtx on every wait, so the old wrapper only added a way to abandon a
	// login that was still running and leave the user typing into nothing.
	err := b.performLogin(loginCtx, chatID, accountID)
	if err == nil {
		return true
	}
	switch {
	case errors.Is(loginCtx.Err(), context.DeadlineExceeded):
		b.send(chatID, MsgLoginTimedOut())
	case errors.Is(loginCtx.Err(), context.Canceled):
		b.send(chatID, MsgLoginCancelled())
	default:
		b.send(chatID, MsgLoginFailed(err.Error()))
	}
	return false
}

func (b *BotService) toggleSelection(chatID int64, idx int, messageID int) {
	acc := b.config.GetActiveAccount()
	if acc == nil {
		return
	}
	patients, _ := acc.Snapshot()
	if idx < 0 || idx >= len(patients) {
		// Stale keyboard from before an account switch or a re-fetch.
		b.send(chatID, MsgStaleKeyboard())
		return
	}

	acc.TogglePatientSelection(patients[idx].OID)
	if err := b.config.Save(b.config.SavePath); err != nil {
		slog.Error("save patient selection failed", "component", "bot", "error", err)
	}
	b.refreshSelectionKeyboard(chatID, messageID, acc)
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
	b.refreshSelectionKeyboard(chatID, messageID, acc)
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
	b.refreshSelectionKeyboard(chatID, messageID, acc)
}

func (b *BotService) refreshSelectionKeyboard(chatID int64, messageID int, acc *config.Account) {
	patients, selected := acc.Snapshot()
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, GetPatientSelectionKeyboard(patients, selected))
	if _, err := b.bot.Send(edit); err != nil {
		slog.Error("refresh selection keyboard failed", "component", "bot", "error", err)
	}
}

// patientNameFor turns an ABHA health id into the name the user picked, so the
// OTP prompt names a person instead of an opaque identifier.
func (b *BotService) patientNameFor(accountID, healthID string) string {
	acc := b.config.GetAccount(accountID)
	if acc == nil {
		return healthID
	}
	for _, p := range acc.GetSelectedPatients() {
		for _, hid := range p.HealthIDs {
			if hid == healthID {
				return p.FLN
			}
		}
	}
	return healthID
}

func (b *BotService) makeGetOTPCallback(ctx context.Context, chatID int64, accountID, label string) func(string) (string, error) {
	return func(healthID string) (string, error) {
		// Serialize OTP collection: only one account may request + wait for its
		// OTP at a time. Otherwise all accounts fire MsgOTPRequired at once and
		// the multi-OTP format is required, which fails when one phone maps to
		// several ABHA accounts.
		// ponytail: single global gate; per-account gates if throughput matters.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
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

		b.sendWithCancel(chatID, MsgOTPRequired(label, b.patientNameFor(accountID, healthID)))

		select {
		case otp := <-otpCh:
			return otp, nil
		case <-time.After(otpInputTimeout):
			return "", fmt.Errorf("OTP timeout")
		case <-ctx.Done():
			// Stop All now actually interrupts a pending OTP prompt. Previously
			// this watched a channel that was always nil during a run, so the
			// waiter survived for 5 minutes and ate every typed message.
			return "", fmt.Errorf("operation cancelled")
		}
	}
}

func (b *BotService) runAllAccounts(ctx context.Context, chatID int64, targetTime time.Time) {
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

	loc := istLoc
	hour, min, sec := 6, 0, 0
	if targetTime.Hour() != 0 || targetTime.Minute() != 0 {
		hour, min, sec = targetTime.Hour(), targetTime.Minute(), targetTime.Second()
	}
	target := time.Date(
		targetTime.Year(), targetTime.Month(), targetTime.Day(),
		hour, min, sec, 0, loc,
	)

	if bookingClosed(target, time.Now()) {
		b.send(chatID, MsgTargetInPast(target))
		return
	}

	totalPatients := 0
	for _, acc := range eligible {
		totalPatients += acc.SelectedCount()
	}
	timeStr := target.Format("15:04") + " IST"
	if !target.After(time.Now()) {
		timeStr = "window open, starting now" // "Today" after 06:00: every account bursts immediately
	}
	b.broadcast(ctx, chatID, MsgExecutionStart(totalPatients, len(eligible), timeStr, target.Format("02 Jan 2006")))

	for _, acc := range eligible {
		label := ""
		if len(eligible) > 1 {
			label = acc.Name
		}
		safego("runAccount", func() { b.runAccount(ctx, chatID, acc, target, label) })
	}
}

// label prefixes this run's messages. It is empty for a single-account run,
// where "[9471392919] " on every line is noise.
func (b *BotService) runAccount(ctx context.Context, chatID int64, acc *config.Account, target time.Time, label string) {
	// Every booking path (date picker, custom date, auto-resume) lands here. A
	// target between 06:00 and 12:00 IST reaches ExecuteTask's "target passed,
	// burst immediately" branch, which is what we want; after 12:00 the day's
	// booking has closed.
	if bookingClosed(target, time.Now()) {
		slog.Warn("refusing booking with past target", "component", "bot",
			"account_key", accountKey(acc.ID), "target", target)
		b.send(chatID, MsgTargetInPast(target))
		return
	}

	runner := newAccountRunner(acc.ID, acc.Name, acc.GetSelectedPatients(), target)
	b.registry.add(runner)
	b.SaveState() // checkpoint while the run is live, so a crash can resume it
	defer func() {
		b.registry.remove(acc.ID)
		b.SaveState()
	}()

	defer func() {
		if r := recover(); r != nil {
			slog.Error("runner panicked, recovered", "component", "bot",
				"account_key", accountKey(acc.ID), "panic", r, "stack", string(debug.Stack()))
			b.broadcast(ctx, chatID, MsgRunnerPanic(label, r))
		}
	}()

	cfg := acc.CloneForABDM(b.config)
	err := runner.start(ctx, cfg, target,
		func(msg string) { b.broadcast(ctx, chatID, MsgRunnerProgress(label, msg)) },
		b.makeGetOTPCallback(ctx, chatID, acc.ID, label),
		b.buildStatusHandlerForAccount(ctx, chatID, label, target),
	)

	if err != nil {
		b.broadcast(ctx, chatID, MsgTaskFailed(label, err))
	} else {
		b.broadcast(ctx, chatID, MsgTaskCompleted(label))
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

	slog.Info("initiating login", "component", "bot", "account_key", accountKey(phone))
	initResp, err := login.InitLogin(ctx, phone)
	if err != nil {
		slog.Error("login init failed", "component", "bot", "account_key", accountKey(phone), "error", err)
		return fmt.Errorf("login init failed: %v", err)
	}
	b.sendWithCancel(chatID, MsgOTPInput(initResp.Hint))

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

	// A single mistyped digit used to abort the login and send the user all the
	// way back to entering their phone number. Re-prompt instead.
	var verifyResp *abdm.LoginVerifyResponse
	for attempt := 1; ; attempt++ {
		var otp string
		select {
		case otp = <-otpCh:
		case <-time.After(otpInputTimeout):
			return fmt.Errorf("OTP input timeout (%s)", otpInputTimeout)
		case <-ctx.Done():
			return ctx.Err()
		}

		slog.Info("verifying OTP", "component", "bot", "account_key", accountKey(phone), "attempt", attempt)
		resp, err := login.VerifyOTP(ctx, initResp.TxnID, otp)
		if err == nil {
			verifyResp = resp
			break
		}
		slog.Warn("OTP verification rejected", "component", "bot", "account_key", accountKey(phone), "attempt", attempt, "error", err)
		if attempt >= maxOTPAttempts {
			return fmt.Errorf("OTP rejected after %d attempts: %v", attempt, err)
		}
		b.sendWithCancel(chatID, MsgOTPRetry(maxOTPAttempts-attempt))
	}

	profiles := verifyResp.ABHAProfiles
	var selectedPHR, selectedName string

	if len(profiles) == 0 {
		return fmt.Errorf("no ABHA profiles found")
	}

	if len(profiles) == 1 {
		selectedPHR, selectedName = profiles[0].ABHAAddress, profiles[0].Name
	} else {
		var lines []string
		for i, p := range profiles {
			verified := "⏳"
			if p.KYCVerified == "VERIFIED" {
				verified = "✅"
			}
			lines = append(lines, MsgProfileLine(i+1, verified, p.Name, p.ABHAAddress))
		}
		// Buttons, not a typed number: this was the last step in the flow that
		// still demanded the user type something that was already on screen.
		labels := make([]string, len(profiles))
		for i, p := range profiles {
			labels[i] = p.Name
		}
		pm := tgbotapi.NewMessage(chatID, MsgMultiProfileSelect(strings.Join(lines, "\n"), len(profiles)))
		pm.ParseMode = tgbotapi.ModeHTML
		pm.ReplyMarkup = GetProfileSelectionKeyboard(labels)
		if _, err := b.bot.Send(pm); err != nil {
			slog.Error("send profile keyboard failed", "component", "bot", "error", err)
		}

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
		case <-time.After(phrInputTimeout):
			return fmt.Errorf("profile selection timeout")
		case <-ctx.Done():
			return ctx.Err()
		}

		idx, err := strconv.Atoi(strings.TrimSpace(phrInput))
		if err != nil || idx < 1 || idx > len(profiles) {
			return fmt.Errorf("invalid selection: %q", phrInput)
		}
		selectedPHR, selectedName = profiles[idx-1].ABHAAddress, profiles[idx-1].Name
	}

	slog.Info("selecting PHR", "component", "bot", "account_key", accountKey(phone))
	phrResp, err := login.SelectPHR(ctx, initResp.TxnID, selectedPHR)
	if err != nil {
		slog.Error("PHR selection failed", "component", "bot", "account_key", accountKey(phone), "error", err)
		return fmt.Errorf("PHR selection failed: %v", err)
	}

	sess, refresh, err := login.ExchangeMinToken(ctx, phrResp.EKA.MinToken, phrResp.EKA.OID)
	if err != nil {
		slog.Error("token exchange failed", "component", "bot", "account_key", accountKey(phone), "error", err)
		return fmt.Errorf("token exchange failed: %v", err)
	}

	slog.Info("login tokens obtained, saving config", "component", "bot", "account_key", accountKey(phone))
	acc.UpdateTokens(sess, refresh)
	if err := b.config.Save(b.config.SavePath); err != nil {
		b.send(chatID, MsgTokensSaveFailed(err.Error()))
	} else {
		slog.Info("login complete", "component", "bot", "account_key", accountKey(phone),
			"token_expires", abdm.ParseJWTExpiry(sess).Format(time.RFC3339))
		b.send(chatID, MsgLoginSuccess(selectedName))
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

	loc := istLoc
	now := time.Now().In(loc)
	targetDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	targetDate = targetDate.AddDate(0, 0, offset)

	b.ClearState()
	b.SaveState()
	safego("runAllAccounts", func() { b.runAllAccounts(ctx, chatID, targetDate) })
}

func (b *BotService) handleDateInput(ctx context.Context, msg *tgbotapi.Message, chatID int64) {
	text := strings.TrimSpace(msg.Text)
	if strings.HasPrefix(text, "/") {
		b.mu.Lock()
		b.waitingForDate = true
		b.pendingDateChat = chatID
		b.mu.Unlock()
		return
	}

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

	loc := istLoc
	targetDate := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, loc)

	b.ClearState()
	b.SaveState()
	safego("runAllAccounts", func() { b.runAllAccounts(ctx, chatID, targetDate) })
}

func (b *BotService) buildStatusHandler(ctx context.Context, chatID int64) abdm.StatusCallback {
	return b.buildStatusHandlerForAccount(ctx, chatID, "", time.Time{})
}

func (b *BotService) buildStatusHandlerForAccount(ctx context.Context, chatID int64, accountName string, targetTime time.Time) abdm.StatusCallback {
	prefix := ""
	if accountName != "" {
		prefix = fmt.Sprintf("[%s] ", accountName)
	}

	// targetDateIST is the canonical appointment date for this run; the bot's
	// spec says "shows on the 13th, gone on the 14th" is the contract.
	// We capture it via closure so the appointment_confirmed sink can stamp
	// each Record with the right target_date_ist even though the OnStatus
	// event payload itself does not carry this field.
	var targetDateIST string
	hipID := b.config.HipID
	if !targetTime.IsZero() {
		targetDateIST = targetTime.Format("2006-01-02")
	}

	send := func(text string) {
		b.broadcast(ctx, chatID, prefix+text)
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
			// L3 sink: persist confirmation for the live-appointments page.
			// health_id is used solely as the dedup primary key and never
			// leaves the host (see PHI rules in pkg/appointments).
			if b.appointments != nil && healthID != "" && targetDateIST != "" {
				if err := b.appointments.Upsert(ctx, appointments.Record{
					HealthID:       healthID,
					TargetDateIST:  targetDateIST,
					HipID:          hipID,
					PatientName:    patient,
					HipName:        hipName,
					TokenNumber:    tokenNum,
					ConfirmedAtUTC: time.Now().UTC(),
				}); err != nil {
					slog.Error("appointments upsert failed", "component", "bot", "error", err)
				}
			}
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
