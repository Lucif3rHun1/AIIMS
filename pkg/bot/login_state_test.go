package bot

import (
	"context"
	"testing"
	"time"
)

// The reported bug: performLogin sent the "enter the OTP" prompt and then
// called SetState(StateLoginOTP), while triggerLogin was waiting on the very
// channel SetState closed. The login cancelled itself ~14ms after prompting,
// so the user never had time to type anything.
func TestSetStateDoesNotCancelInFlightLogin(t *testing.T) {
	t.Chdir(t.TempDir()) // SaveState writes bot_state.json into the cwd

	b := &BotService{
		loginStates: make(map[string]*loginState),
		registry:    newRunnerRegistry(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.loginStates["acct"] = &loginState{phase: "otp", cancel: cancel}

	// Exactly what performLogin does right after prompting for the OTP.
	b.SetState(StateLoginOTP, "acct")

	select {
	case <-ctx.Done():
		t.Fatal("SetState cancelled the in-flight login: the user can never enter the OTP")
	case <-time.After(50 * time.Millisecond):
	}
}

// The other half: an explicit cancel must still reach a waiting login.
func TestResetFlowCancelsInFlightLogin(t *testing.T) {
	t.Chdir(t.TempDir())

	b := &BotService{
		loginStates: make(map[string]*loginState),
		registry:    newRunnerRegistry(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.loginStates["acct"] = &loginState{phase: "otp", cancel: cancel}

	b.resetFlow()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("resetFlow did not cancel the in-flight login")
	}
}

// AutoResume used to fabricate a today-06:00 target when TargetTime was zero,
// which fired a live burst against the wrong date.
func TestRunnerCarriesTargetForResume(t *testing.T) {
	want := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	r := newAccountRunner("id", "name", nil, want)
	if !r.target.Equal(want) {
		t.Fatalf("runner target = %v, want %v", r.target, want)
	}
	if r.startedAt.IsZero() {
		t.Fatal("runner startedAt not recorded; resume cannot tell how long it ran")
	}
}
