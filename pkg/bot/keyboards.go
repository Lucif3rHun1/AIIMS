package bot

import (
	"aiims-appointment/pkg/abdm"
	"fmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func GetMainMenu() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
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
}

func GetSettingsMenu() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔑 Auth Token", "edit_auth_token"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh Token", "edit_refresh_token"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📅 Target Date", "edit_target_date"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📱 Success Phone", "edit_success_phone"),
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

func GetPatientSelectionKeyboard(patients []abdm.Patient, selected map[string]bool) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(patients)+1)

	for i, p := range patients {
		label := fmt.Sprintf("%s (%s)", p.FLN, p.PrimaryHealthID())
		if selected[p.OID] {
			label = "✅ " + label
		} else {
			label = "⬜ " + label
		}

		btn := tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("toggle_%d", i))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(btn))
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("✅ Done", "cmd_done"),
		tgbotapi.NewInlineKeyboardButtonData("selectAll", "cmd_select_all"),
	))

	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}
