package bot

import (
	"aiims-appointment/pkg/config"
	"fmt"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func GetMainMenu() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Fetch Patients", "cmd_fetch"),
			tgbotapi.NewInlineKeyboardButtonData("📊 Status", "cmd_status"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("▶️ Run All Accounts", "cmd_run"),
			tgbotapi.NewInlineKeyboardButtonData("🛑 Stop All", "cmd_stop"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👤 Accounts", "cmd_accounts"),
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", "cmd_settings"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📖 Help", "cmd_help"),
		),
	)
}

func GetSettingsMenu() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏥 HIP ID", "edit_hip_id"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🤖 Bot Token", "edit_bot_token"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👤 Owner ID", "edit_owner_id"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📢 Broadcast Chat ID", "edit_broadcast_chat_id"),
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
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(patients)+2)

	selectedCount := 0
	for _, p := range patients {
		if selected[p.OID] {
			selectedCount++
		}
	}

	for i, p := range patients {
		var label string
		if selected[p.OID] {
			label = fmt.Sprintf("✅ %s − tap to remove", p.FLN)
		} else {
			label = fmt.Sprintf("⬜ %s + tap to add", p.FLN)
		}

		btn := tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("toggle_%d", i))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(btn))
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("✅ Done", "cmd_done"),
		tgbotapi.NewInlineKeyboardButtonData("☑️ Select All", "cmd_select_all"),
	))

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🗑 Clear All", "cmd_clear_sel"),
	))

	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func GetDatePickerKeyboard() tgbotapi.InlineKeyboardMarkup {
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

	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func GetAccountsMenu(accounts []*config.Account, activeID string) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(accounts)+3)

	for _, acc := range accounts {
		label := acc.Name
		if acc.ID == activeID {
			label = "▶️ " + label
		}
		if acc.PatientCount() > 0 {
			label = fmt.Sprintf("%s (%d patients)", label, acc.PatientCount())
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("switch_%s", acc.ID)),
		))
	}

	rows = append(rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ Add Account", "cmd_add_account"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🗑️ Remove Account", "cmd_remove_account"),
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

func GetCancelKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cmd_cancel"),
		),
	)
}
