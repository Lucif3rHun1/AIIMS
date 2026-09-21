package bot

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aiims-appointment/pkg/config"
	"aiims-appointment/pkg/notify"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// fakeAPI records what the bot would send instead of talking to Telegram.
type fakeAPI struct {
	mu   sync.Mutex
	sent []tgbotapi.Chattable
}

func (f *fakeAPI) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, c)
	return tgbotapi.Message{MessageID: len(f.sent)}, nil
}

func (f *fakeAPI) Request(tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	return &tgbotapi.APIResponse{Ok: true}, nil
}

// texts waits until at least n messages have landed, then returns their bodies.
func (f *fakeAPI) texts(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		got := len(f.sent)
		f.mu.Unlock()
		if got >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.sent))
	for _, c := range f.sent {
		if m, ok := c.(tgbotapi.MessageConfig); ok {
			out = append(out, m.Text)
		} else {
			out = append(out, "<non-text>")
		}
	}
	return out
}

func (f *fakeAPI) buttons(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var labels []string
	for _, c := range f.sent {
		m, ok := c.(tgbotapi.MessageConfig)
		if !ok {
			continue
		}
		kb, ok := m.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup)
		if !ok {
			continue
		}
		for _, row := range kb.InlineKeyboard {
			for _, b := range row {
				labels = append(labels, b.Text)
			}
		}
	}
	return labels
}

func newTestBot(t *testing.T) (*BotService, *fakeAPI, *config.Account) {
	t.Helper()
	t.Chdir(t.TempDir()) // SaveState writes bot_state.json into the cwd

	acc := config.NewAccount("9000000001")
	acc.SetName("9000000001")
	cfg := &config.Config{
		TelegramBotToken: "test-token",
		OwnerID:          42,
		HipID:            "HIP1",
		SavePath:         filepath.Join(t.TempDir(), "config.json"),
		Accounts:         map[string]*config.Account{acc.ID: acc},
		ActiveAccountID:  acc.ID,
	}

	api := &fakeAPI{}
	b := &BotService{
		bot:         api,
		config:      cfg,
		notifier:    notify.NewNotifier(nil, 0),
		registry:    newRunnerRegistry(),
		loginStates: make(map[string]*loginState),
		otpWaiters:  make(map[string]chan string),
		sendQueue:   make(chan outgoingMsg, 100),
	}
	b.sendWg.Add(1)
	go b.sendWorker()
	t.Cleanup(func() { close(b.sendQueue); b.sendWg.Wait() })
	return b, api, acc
}

func msg(text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		Text: text,
		From: &tgbotapi.User{ID: 42},
		Chat: &tgbotapi.Chat{ID: 7, Type: "private"},
	}
}

func command(cmd string) *tgbotapi.Message {
	m := msg("/" + cmd)
	m.Entities = []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: len(cmd) + 1}}
	return m
}

func joined(ss []string) string { return strings.Join(ss, "\n---\n") }

// The menu must offer exactly one primary action, and it must be the action
// the account's real state calls for.
func TestMenuPrimaryActionFollowsAccountState(t *testing.T) {
	b, api, acc := newTestBot(t)

	b.handleCommand(context.Background(), command("start"))
	api.texts(t, 1)
	if labels := api.buttons(t); !strings.Contains(joined(labels), "Log in") {
		t.Fatalf("account has no token, expected a Log in button, got %v", labels)
	}

	acc.UpdateTokens("sess", "refresh")
	acc.SetPatients([]config.Patient{{OID: "o1", FLN: "Asha"}, {OID: "o2", FLN: "Bela"}})
	acc.TogglePatientSelection("o1")

	api2 := &fakeAPI{}
	b.bot = api2
	b.handleCommand(context.Background(), command("start"))
	api2.texts(t, 1)
	labels := joined(api2.buttons(t))
	if !strings.Contains(labels, "Book 1 patient") {
		t.Fatalf("1 patient selected, expected a Book button, got %v", labels)
	}
	if strings.Contains(labels, "Run All Accounts") {
		t.Fatalf("stale button name still rendered: %v", labels)
	}
}

// Saving a selection used to emit "N selected." and then a filler "Main Menu".
func TestSaveSelectionEmitsOneMessage(t *testing.T) {
	b, api, acc := newTestBot(t)
	acc.SetPatients([]config.Patient{{OID: "o1", FLN: "Asha"}})
	acc.TogglePatientSelection("o1")

	b.handleCallback(context.Background(), &tgbotapi.CallbackQuery{
		Data:    "cmd_done",
		From:    &tgbotapi.User{ID: 42},
		Message: msg("x"),
	})
	got := api.texts(t, 1)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 message, got %d: %v", len(got), got)
	}
	if strings.Contains(got[0], "Main Menu") {
		t.Fatalf("filler text still sent: %q", got[0])
	}
}

// Cancelling used to emit "Cancelled." and then a second "Main Menu" message.
func TestCancelEmitsOneMessage(t *testing.T) {
	b, api, _ := newTestBot(t)
	b.CancelCurrentFlow(7)
	got := api.texts(t, 1)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 message, got %d: %v", len(got), got)
	}
}

// Picking an ABHA profile by tapping must reach the waiting login.
func TestProfileButtonFeedsWaitingLogin(t *testing.T) {
	b, _, acc := newTestBot(t)
	ls := &loginState{phase: "phr", otpCh: make(chan string, 1), phrCh: make(chan string, 1)}
	b.loginStates[acc.ID] = ls

	b.handleCallback(context.Background(), &tgbotapi.CallbackQuery{
		Data:    "phr_2",
		From:    &tgbotapi.User{ID: 42},
		Message: msg("x"),
	})

	select {
	case got := <-ls.phrCh:
		if got != "3" { // buttons are 0-based, performLogin parses 1-based
			t.Fatalf("phrCh got %q, want \"3\"", got)
		}
	case <-time.After(time.Second):
		t.Fatal("profile tap never reached the waiting login")
	}
}

// Text the bot cannot route must get a reply, not silence.
func TestUnrecognisedTextGetsAReply(t *testing.T) {
	b, api, _ := newTestBot(t)
	b.handleUpdate(context.Background(), tgbotapi.Update{Message: msg("hello there")})
	got := api.texts(t, 1)
	if len(got) == 0 {
		t.Fatal("unrecognised text produced no reply at all")
	}
}

// A run started inside the broadcast group must not see every line twice.
func TestBroadcastDoesNotDoublePostToTheSameChat(t *testing.T) {
	b, api, _ := newTestBot(t)
	b.config.SetBroadcastChatID(7)
	b.notifier = notify.NewNotifier(nil, 7)

	b.broadcast(context.Background(), 7, "run output")

	got := api.texts(t, 1)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 message when chat == broadcast channel, got %d: %v", len(got), got)
	}
}
