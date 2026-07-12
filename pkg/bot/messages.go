package bot

import (
	"fmt"
	"html"
	"strings"
)

// Welcome / Help

func MsgWelcomeNoAccounts() string {
	return "👋 Welcome to AIIMS Appointment Bot!\n\n👤 No accounts yet. Tap '👤 Accounts' to add one and get started."
}

func MsgWelcomeNoActive() string {
	return "👋 Welcome to AIIMS Appointment Bot!\n\n👤 Account exists but none active. Tap '👤 Accounts' to select one."
}

func MsgWelcomeNotLoggedIn(name string) string {
	return "👋 Welcome!\n\n👤 Active: <b>" + html.EscapeString(name) + "</b>\n🔑 Not logged in. Tap '👤 Accounts' to authenticate."
}

func MsgWelcomeLoggedInNoPatients(name, phone string) string {
	return "👋 Welcome!\n\n👤 Active: <b>" + html.EscapeString(name) + "</b>\n📱 " + html.EscapeString(phone) + "\n✅ Logged in.\n\n👥 Tap '🔄 Fetch Patients' to load patient list."
}

func MsgWelcomePatientsNoneSelected(name, phone string, total int) string {
	return "👋 Welcome!\n\n👤 Active: <b>" + html.EscapeString(name) + "</b>\n📱 " + html.EscapeString(phone) + "\n👥 " + fmt.Sprintf("%d", total) + " patients loaded.\n\n👆 Tap '🔄 Fetch Patients' to refresh, then select patients for booking."
}

func MsgWelcomeReadyNoDate(name string, selected, total int) string {
	return fmt.Sprintf("👋 Welcome!\n\n👤 Active: <b>%s</b>\n👥 %d/%d patients selected.\n\n📅 Tap '▶️ Run All Accounts' to choose date and start booking.",
		html.EscapeString(name), selected, total)
}

func MsgWelcomeReady(name string, selected int, targetDate string) string {
	return fmt.Sprintf("👋 Welcome!\n\n👤 Active: <b>%s</b>\n👥 %d patients selected\n📅 Target: %s\n\n🚀 Tap '▶️ Run All Accounts' to start booking.",
		html.EscapeString(name), selected, html.EscapeString(targetDate))
}

func MsgHelp() string {
	return "📖 <b>AIIMS Bot Help</b>\n\n" +
		"<b>Quick Start:</b>\n" +
		"1. 👤 Accounts → ➕ Add Account → Enter phone → OTP → Login\n" +
		"2. 🔄 Fetch Patients → Select patients → ✅ Done\n" +
		"3. ▶️ Run → Choose date → Bot books at 06:00 AM IST\n\n" +
		"<b>Tips:</b>\n" +
		"• Add multiple accounts and run all simultaneously\n" +
		"• Tap ❌ Cancel during any step to go back\n" +
		"• 📊 Status shows all account details\n" +
		"• ⚙️ Settings for HIP ID, bot token, etc."
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
	return "👤 No accounts configured. Use ➕ Add Account to create one."
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

func MsgLoginCompleteFetchPatients() string {
	return "✅ Login complete! You can now fetch patients. Tap 🔄 Fetch Patients to continue."
}

func MsgAccountNotFound() string {
	return "❌ Account not found."
}

func MsgSwitchedAccount(name, phone, hipID string) string {
	return fmt.Sprintf("✅ Switched to <b>%s</b>\n📱 %s\n🏥 HIP: %s",
		html.EscapeString(name), html.EscapeString(phone), html.EscapeString(hipID))
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
	return "❌ Login timed out (7 min)"
}

func MsgLoginCancelled() string {
	return "❌ Login cancelled."
}

func MsgSessionExpired() string {
	return "⚠️ Session expired. Starting fresh login..."
}

func MsgLoginFailedManual() string {
	return "❌ Login failed. Use 🔐 Login to authenticate manually, then try fetching again."
}

func MsgSessionInvalidAfterLogin() string {
	return "❌ Session still invalid after login. Please try /start and authenticate again."
}

func MsgInitiatingLogin(phone string) string {
	return "🔄 Initiating login for " + html.EscapeString(phone) + "..."
}

func MsgOTPInput(hint string) string {
	return "📱 " + html.EscapeString(hint) + "\n\nPlease enter the OTP received:"
}

func MsgAutoSelectedProfile(name, phr string) string {
	return fmt.Sprintf("✅ Auto-selected profile: %s (%s)", html.EscapeString(name), html.EscapeString(phr))
}

func MsgMultiProfileSelect(profiles string, count int) string {
	return fmt.Sprintf("👥 Multiple ABHA profiles found:\n\n%s\n\nEnter the number (1-%d) to select:", profiles, count)
}

func MsgProfileLine(idx int, verified, name, abhaAddress string) string {
	return fmt.Sprintf("%d. %s %s <code>%s</code>", idx, verified, html.EscapeString(name), html.EscapeString(abhaAddress))
}

func MsgSelectedProfile(name, phr string) string {
	return fmt.Sprintf("✅ Selected: %s (%s)", html.EscapeString(name), html.EscapeString(phr))
}

func MsgExchangingTokens() string {
	return "🔄 Exchanging tokens..."
}

func MsgTokensSaveFailed(err string) string {
	return "⚠️ Tokens obtained but config save failed: " + html.EscapeString(err)
}

func MsgLoginSuccess(expiry string) string {
	return "✅ Login successful!\n\n🔑 Session token expires: <code>" + html.EscapeString(expiry) + "</code>\n📝 Config saved.\n\nNext: Tap 🔄 Fetch Patients to load patient list."
}

// OTP

func MsgOTPRequired(accountID, healthID string) string {
	return fmt.Sprintf("🔐 [%s] OTP Required for %s. Please enter the OTP:", html.EscapeString(accountID), html.EscapeString(healthID))
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
	return "❌ No active account. Use 👤 Accounts to add one."
}

func MsgFetchingPatients() string {
	return "⏳ Fetching patients... (Step 1/4)"
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

func MsgPatientsFound(phone string, count, selectedCount, total int) string {
	return fmt.Sprintf("✅ Found <b>%d patients</b> for %s (Step 1/4 complete)\n\n👆 Select/deselect patients below, then tap ✅ Done (Step 2/4)\n<i>%d/%d currently selected</i>",
		count, html.EscapeString(phone), selectedCount, total)
}

func MsgPatientKeyboardFailed() string {
	return "⚠️ Patient list ready but keyboard failed to display. Try /start."
}

func MsgPatientsSelected(count int) string {
	return fmt.Sprintf("✅ %d patient(s) selected. (Step 2/4 complete)\n\n📅 Tap '▶️ Run All Accounts' to choose appointment date. (Step 3/4)", count)
}

func MsgNoPatientsSelected() string {
	return "⚠️ No patients selected. Fetch and select patients first."
}

// Date selection

func MsgNoActiveAccountDate() string {
	return "❌ No active account."
}

func MsgDateStep() string {
	return "📅 Step 3/4: Choose appointment date"
}

func MsgDateSelectTarget() string {
	return "📅 Select target date for execution:"
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

func MsgDateSet(dateStr string) string {
	return "✅ Date set: " + html.EscapeString(dateStr) + " (06:00 AM IST) (Step 3/4 complete)\n\n🚀 Step 4/4: Starting booking..."
}

// Execution / Burst

func MsgNoAccountsConfigured() string {
	return "❌ No accounts configured."
}

func MsgNoPatientsSelectedAny() string {
	return "❌ No patients selected in any account."
}

func MsgExecutionStart(count int, timeStr, dateStr string) string {
	return fmt.Sprintf("🚀 Step 4/4: Starting execution for <b>%d</b> account(s) at %s IST (%s)...", count, html.EscapeString(timeStr), html.EscapeString(dateStr))
}

func MsgRunnerPanic(name string, panicVal interface{}) string {
	return fmt.Sprintf("💥 [%s] Unexpected error (recovered): %v", html.EscapeString(name), panicVal)
}

func MsgTaskFailed(name string, err error) string {
	return fmt.Sprintf("❌ [%s] Task failed: %v", html.EscapeString(name), err)
}

func MsgTaskCompleted(name string) string {
	return fmt.Sprintf("✅ [%s] Task completed.", html.EscapeString(name))
}

func MsgRunnerProgress(name, msg string) string {
	return fmt.Sprintf("[%s] %s", html.EscapeString(name), msg)
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
