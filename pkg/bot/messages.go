package bot

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// tag renders an account label as a "[name] " prefix, or nothing when the
// label is empty. With a single account running, the prefix is pure noise.
// plural avoids the "1 patient(s)" look.
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func tag(label string) string {
	if label == "" {
		return ""
	}
	return "[" + html.EscapeString(label) + "] "
}

// accountSuffix names the account count only when it is worth saying.
func accountSuffix(accounts int) string {
	if accounts <= 1 {
		return ""
	}
	return fmt.Sprintf(" across %d accounts", accounts)
}

func MsgChooseProfileExpired() string {
	return "⚠️ That profile list has expired. Start the login again."
}

// Welcome / Help

func MsgWelcomeNoAccounts() string {
	return "👋 Welcome to AIIMS Appointment Bot!\n\n👤 No accounts yet. Tap ➕ Add account to get started."
}

func MsgWelcomeNoActive() string {
	return "👋 Welcome to AIIMS Appointment Bot!\n\n👤 Tap 👤 Accounts to pick one."
}

func MsgWelcomeNotLoggedIn(name string) string {
	return "👋 Welcome!\n\n👤 Active: <b>" + html.EscapeString(name) + "</b>\n🔑 Not logged in. Tap 🔐 Log in."
}

func MsgWelcomeLoggedInNoPatients(name, phone string) string {
	return "👋 Welcome!\n\n👤 Active: <b>" + html.EscapeString(name) + "</b>\n📱 " + html.EscapeString(phone) + "\n✅ Logged in.\n\n👥 Tap 🔄 Load patients."
}

func MsgWelcomePatientsNoneSelected(name, phone string, total int) string {
	return "👋 Welcome!\n\n👤 Active: <b>" + html.EscapeString(name) + "</b>\n📱 " + html.EscapeString(phone) + "\n👥 " + fmt.Sprintf("%d", total) + " patients loaded.\n\n👆 Tap 👥 Choose patients to pick who to book for."
}

func MsgWelcomeReadyNoDate(name string, selected, total int) string {
	return fmt.Sprintf("👋 Welcome!\n\n👤 Active: <b>%s</b>\n👥 %d/%d patients selected.\n\n📅 Tap <b>Book</b> to pick a date and start.",
		html.EscapeString(name), selected, total)
}

func MsgWelcomeReady(name string, selected int, targetDate string) string {
	return fmt.Sprintf("👋 Welcome!\n\n👤 Active: <b>%s</b>\n👥 %d patients selected\n📅 Target: %s\n\n🚀 Tap 📅 Book to choose a date and start.",
		html.EscapeString(name), selected, html.EscapeString(targetDate))
}

func MsgHelp() string {
	return "📖 <b>AIIMS Bot Help</b>\n\n" +
		"<b>Quick start</b> — the menu always shows your next step:\n" +
		"• ➕ Add account → enter phone → enter the OTP you receive\n" +
		"• 🔄 Load patients → 👥 Choose patients → 💾 Save\n" +
		"• 📅 Book → pick a date → the bot books at 06:00 AM IST (today works until 12:00 PM, booked immediately)\n\n" +
		"<b>Tips:</b>\n" +
		"• Add multiple accounts and book them all at once\n" +
		"• Send /cancel at any point to get unstuck\n" +
		"• 📊 Status shows all account details\n" +
		"• ⚙️ Settings holds the HIP ID and broadcast chat"
}

// Cancel

func MsgCancelled() string {
	return "❌ Cancelled. What would you like to do?"
}

// Auth

func MsgAuthMustBeGroup() string {
	return "⚠️ /auth must be used in a group or channel."
}

func MsgAuthFailedSave(err string) string {
	return "❌ Failed to save: " + html.EscapeString(err)
}

func MsgAuthRegistered(chatID int64) string {
	return fmt.Sprintf("✅ Broadcast channel registered! Chat ID: <code>%d</code>", chatID)
}

func MsgAuthChannelRegistered() string {
	return "📡 <b>Channel Registered</b>\n\nAll appointment status updates will be sent here."
}

// Stop

func MsgAllStopped() string {
	return "🛑 All tasks stopped."
}

func MsgStoppingAll() string {
	return "🛑 Stopping all tasks..."
}

func MsgNoTasksRunning() string {
	return "ℹ️ No tasks running."
}

// Account management

func MsgNoAccounts() string {
	return "👤 No accounts yet.\n\nTap ➕ Add account to get started."
}

func MsgAccountsMenu() string {
	return "👤 <b>Accounts</b>\n\nSelect an account to activate:"
}

func MsgEnterPhone() string {
	return "📱 Enter the phone number for the new account (e.g., 9876543210):"
}

func MsgInvalidPhone() string {
	return "❌ Invalid phone number. Try again:"
}

func MsgAccountAdded(phone string) string {
	return "✅ Account <b>" + html.EscapeString(phone) + "</b> added and activated!\n\n🔐 Initiating login..."
}

func MsgAccountNotFound() string {
	return "❌ Account not found."
}

func MsgNoAccountsToRemove() string {
	return "👤 No accounts to remove."
}

func MsgCannotRemoveOnlyAccount() string {
	return "⚠️ Cannot remove the only account. Add another first."
}

func MsgRemoveAccountMenu() string {
	return "🗑️ <b>Remove Account</b>\n\nSelect an account to remove:"
}

func MsgAccountRemoved() string {
	return "✅ Account removed."
}

// Login flow

func MsgLoginAlreadyInProgress() string {
	return "⚠️ Login already in progress for this account."
}

func MsgLoginFailed(err string) string {
	return "❌ Login failed: " + html.EscapeString(err)
}

func MsgLoginTimedOut() string {
	return "❌ Login timed out.\n\nTap 🔐 Log in to start over."
}

func MsgLoginCancelled() string {
	return "❌ Login cancelled."
}

func MsgSessionExpired() string {
	return "⚠️ Session expired — signing in again."
}

func MsgLoginFailedManual() string {
	return "❌ Login failed.\n\nTap 🔐 Log in to try again."
}

func MsgSessionInvalidAfterLogin() string {
	return "❌ Session still invalid.\n\nTap 🔐 Log in to retry."
}

func MsgOTPInput(hint string) string {
	return "📱 " + html.EscapeString(hint) + "\n\nPlease enter the OTP received:"
}

func MsgMultiProfileSelect(profiles string, count int) string {
	return fmt.Sprintf("👥 <b>%d ABHA profiles</b> on this number:\n\n%s\n\n👆 Tap the one to use:", count, profiles)
}

func MsgProfileLine(idx int, verified, name, abhaAddress string) string {
	return fmt.Sprintf("%d. %s %s <code>%s</code>", idx, verified, html.EscapeString(name), html.EscapeString(abhaAddress))
}

func MsgTokensSaveFailed(err string) string {
	return "⚠️ Tokens obtained but config save failed: " + html.EscapeString(err)
}

func MsgLoginSuccess(profileName string) string {
	return "✅ Signed in as <b>" + html.EscapeString(profileName) + "</b>"
}

// OTP

func MsgOTPRequired(label, patientName string) string {
	return fmt.Sprintf("🔐 %sOTP required for <b>%s</b>\n\nEnter the 6-digit code:", tag(label), html.EscapeString(patientName))
}

func MsgOTPChannelBusy() string {
	return "⚠️ OTP channel busy, try again."
}

func MsgLoginOTPChannelBusy() string {
	return "⚠️ Login OTP channel busy, try again."
}

func MsgSelectionChannelBusy() string {
	return "⚠️ Selection channel busy, try again."
}

func MsgMultiOTPNeeded(accountIDs []string, exampleID string) string {
	var lines []string
	for _, id := range accountIDs {
		lines = append(lines, "• <code>"+html.EscapeString(id)+"</code>")
	}
	return fmt.Sprintf("⚠️ Multiple accounts need OTP:\n\n%s\n\nReply with: <code>OTP AccountID Code</code>\nExample: <code>%s 123456</code>",
		strings.Join(lines, "\n"), html.EscapeString(exampleID))
}

// Patient management

func MsgNoActiveAccount() string {
	return "❌ No active account.\n\nTap 👤 Accounts to pick or add one."
}

func MsgFetchingPatients() string {
	return "⏳ Fetching patients..."
}

func MsgFetchInternalError() string {
	return "❌ Internal error during fetch. Please try again."
}

func MsgFetchFailed(err string) string {
	return "❌ Failed to fetch patients: " + html.EscapeString(err)
}

func MsgFetchSaveFailed(err string) string {
	return "⚠️ Patients fetched but save failed: " + html.EscapeString(err)
}

// The live count lives on the Save button only. It used to also sit in this
// text, which never re-rendered on a toggle, so the sentence and the keyboard
// drifted apart ("3/4 selected" above a keyboard showing 1).
func MsgPatientsFound(count int) string {
	return fmt.Sprintf("✅ Found <b>%d patients</b>.\n\n👆 Tap names to select, then 💾 Save.", count)
}

func MsgPatientKeyboardFailed() string {
	return "⚠️ Patient list ready but keyboard failed to display. Try /start."
}

func MsgPatientsSelected(count int) string {
	return fmt.Sprintf("✅ Saved — <b>%s</b> selected.", plural(count, "patient"))
}

func MsgNoPatientsSelected() string {
	return "⚠️ No patients selected. Fetch and select patients first."
}

// Date selection

func MsgNoActiveAccountDate() string {
	return "❌ No active account."
}

func MsgDateSelectTarget() string {
	return "📅 Choose the appointment date:"
}

func MsgDateCustomFormat() string {
	return "📝 Enter date in DD-MM-YYYY format (e.g. 18-05-2026):"
}

func MsgDateInvalidFormat() string {
	return "❌ Invalid format. Use DD-MM-YYYY (e.g. 18-05-2026). Try again:"
}

func MsgDateInvalid() string {
	return "❌ Invalid date selection."
}

func MsgTasksAlreadyRunning() string {
	return "⚠️ Tasks already running. Stop them first."
}

// Execution / Burst

func MsgNoAccountsConfigured() string {
	return "❌ No accounts configured."
}

func MsgNoPatientsSelectedAny() string {
	return "❌ No patients selected in any account."
}

func MsgExecutionStart(patients, accounts int, timeStr, dateStr string) string {
	return fmt.Sprintf("🚀 Booking <b>%s</b>%s\n📅 %s at %s IST", plural(patients, "patient"), accountSuffix(accounts), html.EscapeString(dateStr), html.EscapeString(timeStr))
}

func MsgRunnerPanic(name string, panicVal interface{}) string {
	return fmt.Sprintf("%s💥 Unexpected error (recovered): %v", tag(name), panicVal)
}

func MsgTaskFailed(name string, err error) string {
	return fmt.Sprintf("%s❌ Failed: %v", tag(name), err)
}

func MsgTaskCompleted(name string) string {
	return tag(name) + "✅ Booking run finished."
}

func MsgRunnerProgress(name, msg string) string {
	return tag(name) + msg
}

// Settings

func MsgSettingsMenu() string {
	return "⚙️ Settings Menu"
}

func MsgFieldEditor(field, currentVal string) string {
	return fmt.Sprintf("🔧 <b>%s</b>\nCurrent: <code>%s</code>", html.EscapeString(field), html.EscapeString(currentVal))
}

func MsgEnterNewValue(field string) string {
	return "✏️ Enter new value for " + html.EscapeString(field) + ":"
}

func MsgInvalidOwnerID() string {
	return "❌ Invalid number for Owner ID"
}

func MsgInvalidBroadcastID() string {
	return "❌ Invalid number for Broadcast Chat ID"
}

func MsgConfigSaveFailed(err string) string {
	return "❌ Failed to save config: " + html.EscapeString(err)
}

func MsgConfigSaved() string {
	return "✅ Config saved successfully!"
}

func MsgSaveFailed(err string) string {
	return "❌ Failed to save: " + html.EscapeString(err)
}

// Status

func MsgNoAccountsStatus() string {
	return "👤 No accounts configured."
}

func MsgStatusHeader() string {
	return "📊 <b>Account Status</b>\n"
}

func MsgStatusAccount(name, status, phone, hipID, targetDate string, selected, total int) string {
	return fmt.Sprintf("👤 <b>%s</b> %s\n📱 %s | 🏥 %s\n📅 %s | ✅ %d/%d selected",
		html.EscapeString(name), status, html.EscapeString(phone), html.EscapeString(hipID),
		html.EscapeString(targetDate), selected, total)
}

// Status handler (burst callbacks)

func MsgWaitingForTarget(waitSeconds float64, target string) string {
	return fmt.Sprintf("⏳ <b>Waiting for Target Time</b>\n\nWaiting %.0f seconds until %s", waitSeconds, html.EscapeString(target))
}

func MsgPrewarmStarted(totalPatients int, tminus string) string {
	return fmt.Sprintf("🔥 <b>Pre-Warming Started</b>\n\n👥 %d patients\n⏱️ T-minus: %s", totalPatients, html.EscapeString(tminus))
}

func MsgPrewarmTokenRefreshed(totalPatients int) string {
	return fmt.Sprintf("🔑 <b>Master Token Refreshed</b>\n\nPre-switching %d patient tokens...", totalPatients)
}

func MsgPrewarmTokenReady(patient string, ready, total int) string {
	return fmt.Sprintf("✅ <b>Token Ready</b>: <code>%s</code> (%d/%d)", html.EscapeString(patient), ready, total)
}

func MsgPrewarmComplete(ready, failed, total int) string {
	return fmt.Sprintf("🔥 <b>Pre-Warm Complete</b>\n\n✅ Ready: %d\n❌ Failed: %d\n👥 Total: %d\n\n⏳ Waiting for T-0...", ready, failed, total)
}

// MsgUnrecognizedInput replies to text the bot was not expecting, so a typed
// message is never silently swallowed.
func MsgUnrecognizedInput() string {
	return "🤔 I wasn't expecting that.\n\nUse the buttons below, or send /help to see what I can do."
}

// MsgStaleKeyboard covers a keyboard rendered before the patient list changed.
func MsgStaleKeyboard() string {
	return "⚠️ That patient list is out of date.\n\nTap 🔄 to reload it."
}

// MsgTargetInPast refuses a booking whose slot has already passed.
func MsgTargetInPast(t time.Time) string {
	return fmt.Sprintf("⏰ <b>Booking for %s has closed.</b>\n\nTokens can be booked until 12:00 PM IST that day.\n\nPick a later date.",
		html.EscapeString(t.Format("02 Jan 2006")))
}

// MsgOTPRetry re-prompts after a rejected OTP instead of killing the login.
func MsgOTPRetry(left int) string {
	return fmt.Sprintf("❌ That OTP was not accepted.\n\nPlease re-enter the 6-digit code — <b>%d attempt(s) left</b>.", left)
}

// MsgResumeSkipped explains why an interrupted booking was not auto-resumed.
func MsgResumeSkipped(account string, target time.Time) string {
	if target.IsZero() {
		return fmt.Sprintf("⚠️ Could not auto-resume <b>%s</b>: the booking slot was not recorded.\n\nStart it again with 📅 Book.",
			html.EscapeString(account))
	}
	return fmt.Sprintf("⚠️ Could not auto-resume <b>%s</b>: its slot (%s) has already passed.\n\nStart it again with 📅 Book.",
		html.EscapeString(account), html.EscapeString(target.Format("02 Jan 2006, 15:04")))
}

func MsgUnknownCommand(cmd string) string {
	return "🤔 Unknown command /" + html.EscapeString(cmd) + ".\n\nTry /start, /status, /cancel, /stop or /help."
}

func MsgNoPatientsLoaded() string {
	return "👥 No patients loaded yet.\n\nTap 🔄 to load them from your ABHA account first."
}

// No count here on purpose: this text is sent once and never re-rendered, while
// the Save button below it updates on every tap. Two owners, guaranteed drift.
func MsgChoosePatients(selected, total int) string {
	return "👥 <b>Choose patients</b>\n\nTap a name to add or remove it, then 💾 Save."
}

func MsgAccountExistsReLogin(phone string) string {
	return "👤 <b>" + html.EscapeString(phone) + "</b> already exists — keeping its patients and selections.\n\n🔐 Logging in again..."
}

func MsgFieldNotEditable(field string) string {
	return "🔒 <b>" + html.EscapeString(field) + "</b> cannot be changed from chat.\n\nEdit config.json and restart the bot."
}

func MsgInvalidFieldValue(field, reason string) string {
	return "❌ Could not set <b>" + html.EscapeString(field) + "</b>: " + html.EscapeString(reason) + "\n\nTry again, or send /cancel."
}
