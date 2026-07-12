package notify

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/time/rate"
)

const (
	maxRetries     = 3
	retryBaseDelay = 500 * time.Millisecond
)

type Notifier struct {
	bot     *tgbotapi.BotAPI
	chatID  int64
	limiter *rate.Limiter
}

func NewNotifier(bot *tgbotapi.BotAPI, chatID int64) *Notifier {
	return &Notifier{
		bot:     bot,
		chatID:  chatID,
		limiter: rate.NewLimiter(rate.Every(30*time.Millisecond), 5),
	}
}

func (n *Notifier) UpdateChatID(chatID int64) {
	n.chatID = chatID
}

func (n *Notifier) HasChannel() bool {
	return n.chatID != 0
}

func (n *Notifier) Send(ctx context.Context, text string, parseMode string) error {
	if n.chatID == 0 {
		return fmt.Errorf("no broadcast channel configured")
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := n.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("rate limiter cancelled: %v", err)
		}

		msg := tgbotapi.NewMessage(n.chatID, text)
		if parseMode != "" {
			msg.ParseMode = parseMode
		}

		if _, err := n.bot.Send(msg); err != nil {
			lastErr = err
			delay := retryBaseDelay * time.Duration(1<<attempt)
			slog.Warn("send attempt failed", "component", "notify", "attempt", attempt, "error", err, "retry_after", delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				continue
			}
		}
		return nil
	}
	return lastErr
}

func (n *Notifier) SendHTML(ctx context.Context, text string) error {
	return n.Send(ctx, text, tgbotapi.ModeHTML)
}

func FormatAppointmentSuccess(patientName, healthID, tokenNumber, hipName, tokenExpiry, footer string) string {
	return fmt.Sprintf(
		"✅ <b>Appointment Confirmed</b>\n\n"+
			"👤 Patient: <code>%s</code>\n"+
			"🏥 Hospital: %s\n"+
			"🎫 Token: <code>%s</code>\n"+
			"⏰ Valid Until: %s\n"+
			"📋 %s",
		html.EscapeString(patientName), html.EscapeString(hipName),
		html.EscapeString(tokenNumber), html.EscapeString(tokenExpiry),
		html.EscapeString(footer),
	)
}

func FormatQueueUpdate(patientName string, queuePos, totalInQueue int) string {
	return fmt.Sprintf(
		"⏳ <b>Queue Update</b>\n\n"+
			"👤 Patient: <code>%s</code>\n"+
			"📊 Position: %d / %d",
		html.EscapeString(patientName), queuePos, totalInQueue,
	)
}

func FormatValidationStatus(patientName string, validated, total int, errMsg string) string {
	if errMsg != "" {
		return fmt.Sprintf(
			"❌ <b>Validation Failed</b>\n\n"+
				"👤 Patient: <code>%s</code>\n"+
				"⚠️ Error: %s",
			html.EscapeString(patientName), html.EscapeString(errMsg),
		)
	}
	return fmt.Sprintf(
		"🔐 <b>Validation Progress</b>\n\n"+
			"👤 Patient: <code>%s</code>\n"+
			"✅ Validated: %d / %d",
		html.EscapeString(patientName), validated, total,
	)
}

func FormatBurstResult(patientName string, success bool, tokenNumber string) string {
	if success {
		return FormatAppointmentSuccess(
			patientName, "", tokenNumber,
			"AIIMS Raipur", "", "Proceed to counter for OPD slip",
		)
	}
	return fmt.Sprintf(
		"❌ <b>Booking Failed</b>\n\n"+
			"👤 Patient: <code>%s</code>\n"+
			"⚠️ All burst attempts exhausted",
		html.EscapeString(patientName),
	)
}

func FormatExecutionStart(totalPatients int, targetTime string) string {
	return fmt.Sprintf(
		"🚀 <b>Execution Started</b>\n\n"+
			"👥 Patients: %d\n"+
			"🎯 Target: %s\n"+
			"⏰ Time: %s",
		totalPatients, html.EscapeString(targetTime), time.Now().Format("02-01-2006 15:04:05"),
	)
}

func FormatExecutionComplete(successCount, failCount int32, total int) string {
	rate := float64(0)
	if total > 0 {
		rate = float64(successCount) / float64(total) * 100
	}
	return fmt.Sprintf(
		"📊 <b>Execution Complete</b>\n\n"+
			"✅ Success: %d\n"+
			"❌ Failed: %d\n"+
			"👥 Total: %d\n"+
			"📈 Rate: %.0f%%",
		successCount, failCount, total, rate,
	)
}

func BuildStatusMessage(validated, total int, successCount, failCount int32, running bool) string {
	if !running {
		return "🟢 <b>System Online</b>\n\nIdle — no task running."
	}
	return fmt.Sprintf(
		"🟡 <b>Task Running</b>\n\n"+
			"✅ Validated: %d / %d\n"+
			"🎫 Appointments: %d\n"+
			"❌ Failed: %d",
		validated, total, successCount, failCount,
	)
}
