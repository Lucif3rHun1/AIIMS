package notify

import (
	"context"
	"fmt"
	"log"
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
			log.Printf("[notify] attempt %d failed: %v, retrying in %v", attempt, err, delay)
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

func (n *Notifier) SendMarkdown(ctx context.Context, text string) error {
	return n.Send(ctx, text, tgbotapi.ModeMarkdown)
}

func FormatAppointmentSuccess(patientName, healthID, tokenNumber, hipName, tokenExpiry, footer string) string {
	return fmt.Sprintf(
		"✅ *Appointment Confirmed*\n\n"+
			"👤 Patient: `%s`\n"+
			"🏥 Hospital: %s\n"+
			"🎫 Token: `%s`\n"+
			"⏰ Valid Until: %s\n"+
			"📋 %s",
		patientName, hipName, tokenNumber, tokenExpiry, footer,
	)
}

func FormatQueueUpdate(patientName string, queuePos, totalInQueue int) string {
	return fmt.Sprintf(
		"⏳ *Queue Update*\n\n"+
			"👤 Patient: `%s`\n"+
			"📊 Position: %d / %d",
		patientName, queuePos, totalInQueue,
	)
}

func FormatValidationStatus(patientName string, validated, total int, errMsg string) string {
	if errMsg != "" {
		return fmt.Sprintf(
			"❌ *Validation Failed*\n\n"+
				"👤 Patient: `%s`\n"+
				"⚠️ Error: %s",
			patientName, errMsg,
		)
	}
	return fmt.Sprintf(
		"🔐 *Validation Progress*\n\n"+
			"👤 Patient: `%s`\n"+
			"✅ Validated: %d / %d",
		patientName, validated, total,
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
		"❌ *Booking Failed*\n\n"+
			"👤 Patient: `%s`\n"+
			"⚠️ All burst attempts exhausted",
		patientName,
	)
}

func FormatExecutionStart(totalPatients int, targetTime string) string {
	return fmt.Sprintf(
		"🚀 *Execution Started*\n\n"+
			"👥 Patients: %d\n"+
			"🎯 Target: %s\n"+
			"⏰ Time: %s",
		totalPatients, targetTime, time.Now().Format("02-01-2006 15:04:05"),
	)
}

func FormatExecutionComplete(successCount, failCount int32, total int) string {
	return fmt.Sprintf(
		"📊 *Execution Complete*\n\n"+
			"✅ Success: %d\n"+
			"❌ Failed: %d\n"+
			"👥 Total: %d\n"+
			"📈 Rate: %.0f%%",
		successCount, failCount, total,
		float64(successCount)/float64(total)*100,
	)
}

func BuildStatusMessage(validated, total int, successCount, failCount int32, running bool) string {
	if !running {
		return "🟢 *System Online*\n\nIdle — no task running."
	}
	return fmt.Sprintf(
		"🟡 *Task Running*\n\n"+
			"✅ Validated: %d / %d\n"+
			"🎫 Appointments: %d\n"+
			"❌ Failed: %d",
		validated, total, successCount, failCount,
	)
}
