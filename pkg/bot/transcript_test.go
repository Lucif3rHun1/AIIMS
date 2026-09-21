package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"aiims-appointment/pkg/config"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func show(t *testing.T, api *fakeAPI, header string) {
	t.Helper()
	api.texts(t, 1)
	api.mu.Lock()
	defer api.mu.Unlock()
	t.Logf("===== %s =====", header)
	for _, c := range api.sent {
		m, ok := c.(tgbotapi.MessageConfig)
		if !ok {
			t.Log("  [keyboard edit]")
			continue
		}
		t.Logf("  MSG: %s", strings.ReplaceAll(m.Text, "\n", "\n       "))
		if kb, ok := m.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup); ok {
			for _, row := range kb.InlineKeyboard {
				var labs []string
				for _, b := range row {
					labs = append(labs, b.Text)
				}
				t.Logf("       [ %s ]", strings.Join(labs, " | "))
			}
		}
	}
	api.sent = nil
}

// TestTranscript renders the whole UI in one place:
//
//	go test ./pkg/bot/ -run TestTranscript -v
//
// It asserts nothing; it exists so copy can be reviewed without a phone.
func TestTranscript(t *testing.T) {
	b, api, acc := newTestBot(t)
	acc.UpdateTokens("sess", "refresh")
	acc.SetPatients([]config.Patient{
		{OID: "o1", FLN: "Anupam Devi"}, {OID: "o2", FLN: "Jay Shankar"},
		{OID: "o3", FLN: "Tilak Raj"}, {OID: "o4", FLN: "Tushar Raj"},
	})
	acc.TogglePatientSelection("o2")
	ctx := context.Background()
	cb := func(data string) {
		b.handleCallback(ctx, &tgbotapi.CallbackQuery{
			Data: data, From: &tgbotapi.User{ID: 42}, Message: msg("x")})
	}

	b.handleCommand(ctx, command("start"))
	show(t, api, "/start")
	cb("cmd_select")
	show(t, api, "tap: Choose patients")
	cb("cmd_done")
	show(t, api, "tap: Save")
	cb("cmd_run")
	show(t, api, "tap: Book")

	target := time.Date(2026, 9, 22, 6, 0, 0, 0, istLoc)
	t.Log("===== booking phase copy =====")
	for _, s := range []string{
		MsgExecutionStart(1, 1, target.Format("15:04"), target.Format("02 Jan 2006")),
		MsgOTPRequired("", "Jay Shankar"),
		MsgRunnerProgress("", "⏳ Waiting 3h45m0s until target time..."),
		MsgTaskCompleted(""),
		MsgSessionExpired(),
		MsgLoginSuccess("Tushar Raj"),
		MsgPatientsFound(4),
	} {
		t.Logf("  MSG: %s", strings.ReplaceAll(s, "\n", "\n       "))
	}
}
