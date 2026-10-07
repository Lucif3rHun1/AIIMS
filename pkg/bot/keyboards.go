package bot

import (
	"aiims-appointment/pkg/config"
	"fmt"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// GetMainMenu renders ONE primary action matching the account's real state.
// The old fixed grid offered Fetch and Run regardless of state and had no
// login button at all — while error messages told the user to press one.
func GetMainMenu(acc *config.Account, accountCount int, running bool) tgbotapi.InlineKeyboardMarkup {
	btn := tgbotapi.NewInlineKeyboardButtonData
	row := tgbotapi.NewInlineKeyboardRow
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, 4)

	switch {
	case running:
		rows = append(rows, row(btn("🛑 Stop booking", "cmd_stop")), row(btn("📊 Progress", "cmd_status")))
	case accountCount == 0:
		rows = append(rows, row(btn("➕ Add account", "cmd_add_account")))
	case acc == nil:
		rows = append(rows, row(btn("👤 Pick an account", "cmd_accounts")))
	default:
		auth, _ := acc.Tokens()
		total, selected := acc.PatientCount(), acc.SelectedCount()
		switch {
		case auth == "":
			rows = append(rows, row(btn("🔐 Log in ("+acc.Name+")", "cmd_login")))
		case total == 0:
			rows = append(rows, row(btn("🔄 Load patients", "cmd_fetch")))
		case selected == 0:
			rows = append(rows, row(btn("👥 Choose patients", "cmd_select")), row(btn("🔄 Reload patients", "cmd_fetch")))
		default:
			rows = append(rows,
				row(btn(fmt.Sprintf("📅 Book %s", plural(selected, "patient")), "cmd_run")),
				row(btn(fmt.Sprintf("👥 Patients (%d/%d)", selected, total), "cmd_select"), btn("🔄 Reload", "cmd_fetch")),
			)
		}
	}

	if accountCount > 0 && !running {
		rows = append(rows, row(btn("👤 Accounts", "cmd_accounts"), btn("📊 Status", "cmd_status")))
	}
	rows = append(rows, row(btn("⚙️ Settings", "cmd_settings"), btn("📖 Help", "cmd_help")))
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// GetSettingsMenu deliberately omits bot token and owner ID. The token
// authenticates this process (editing it over chat cannot take effect without
// a restart, and echoing it leaks the secret), and a mistyped owner ID locks
// the owner out with no in-chat recovery. Both stay editable in config.json.
func GetSettingsMenu() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏥 HIP ID", "edit_hip_id"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📢 Broadcast chat", "edit_broadcast_chat_id"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "cmd_start"),
		),
	)
}

func GetFieldEditMenu(fieldName string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✏️ Edit Value", fmt.Sprintf("start_edit_%s", fieldName)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "cmd_settings"),
		),
	)
}

func GetPatientSelectionKeyboard(patients []config.Patient, selected map[string]bool) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(patients)+3)

	selectedCount := 0
	for _, p := range patients {
		if selected[p.OID] {
			selectedCount++
		}
	}

	for i, p := range patients {
		// Just the tick and the name: the old "− tap to remove" suffix pushed
		// real patient names off-screen on a phone.
		mark := "⬜"
		if selected[p.OID] {
			mark = "✅"
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(mark+" "+p.FLN, fmt.Sprintf("toggle_%d", i)),
		))
	}

	rows = append(rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("💾 Save (%d selected)", selectedCount), "cmd_done"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Select all", "cmd_select_all"),
			tgbotapi.NewInlineKeyboardButtonData("Clear all", "cmd_clear_sel"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "cmd_cancel"),
		),
	)

	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// bookingCloseHour is when the day's token booking closes (IST). Bookings fire
// at 06:00; picking the day between 06:00 and 12:00 bursts immediately.
const bookingCloseHour = 12

// bookingClosed reports whether day's booking window has closed as of now.
func bookingClosed(day, now time.Time) bool {
	d := day.In(istLoc)
	return !now.Before(time.Date(d.Year(), d.Month(), d.Day(), bookingCloseHour, 0, 0, 0, istLoc))
}

func GetDatePickerKeyboard() tgbotapi.InlineKeyboardMarkup {
	loc := istLoc
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	// Token booking stays open until 12:00 IST; after that runAccount refuses
	// "Today", so offering it is a button that only ever produces an error.
	start := 0
	if bookingClosed(today, now) {
		start = 1
	}

	rows := make([][]tgbotapi.InlineKeyboardButton, 0, 6)
	for i := 0; i < 8; i += 2 {
		r := make([]tgbotapi.InlineKeyboardButton, 0, 2)
		for col := 0; col < 2; col++ {
			offset := start + i + col
			d := today.AddDate(0, 0, offset)
			label := d.Format("02 Jan (Mon)")
			switch offset {
			case 0:
				label = "📌 Today " + d.Format("02 Jan")
			case 1:
				label = "📅 Tomorrow " + d.Format("02 Jan")
			}
			r = append(r, tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("date_%d", offset)))
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(r...))
	}

	rows = append(rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📝 Custom Date (DD-MM-YYYY)", "date_custom"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "cmd_cancel"),
		),
	)
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func GetAccountsMenu(accounts []*config.Account, activeID string) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(accounts)+4)

	needsLogin := false
	for _, acc := range accounts {
		label := acc.Name
		if auth, _ := acc.Tokens(); auth == "" {
			label += " · needs login"
			if acc.ID == activeID {
				needsLogin = true
			}
		} else {
			label += fmt.Sprintf(" · %d/%d", acc.SelectedCount(), acc.PatientCount())
		}
		if acc.ID == activeID {
			label = "• " + label + " (active)"
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("switch_%s", acc.ID)),
		))
	}

	if len(accounts) > 0 {
		verb := "🔐 Re-login active account"
		if needsLogin {
			verb = "🔐 Log in to active account"
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(verb, "cmd_login"),
		))
	}

	rows = append(rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ Add account", "cmd_add_account"),
			tgbotapi.NewInlineKeyboardButtonData("🗑️ Remove", "cmd_remove_account"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "cmd_start"),
		),
	)

	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func GetRemoveAccountMenu(accounts []*config.Account, activeID string) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(accounts)+1)

	for _, acc := range accounts {
		label := "🗑️ " + acc.Name
		if acc.ID == activeID {
			label += " (active)"
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("removeacc_%s", acc.ID)),
		))
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Cancel", "cmd_accounts"),
	))

	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// GetProfileSelectionKeyboard turns ABHA profile picking into taps. It was the
// only step in the whole flow that still demanded a typed number.
func GetProfileSelectionKeyboard(labels []string) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(labels)+1)
	for i, label := range labels {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("phr_%d", i)),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cmd_cancel"),
	))
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func GetCancelKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cmd_cancel"),
		),
	)
}
