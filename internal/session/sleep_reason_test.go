package session

import (
	"reflect"
	"testing"
	"time"
)

// TestSleepReasonConstantValues pins the on-store string of every sleep reason
// so a constant edit that would change the wire value is caught here rather
// than silently reclassifying live beads.
func TestSleepReasonConstantValues(t *testing.T) {
	want := map[SleepReason]string{
		SleepReasonIdle:                  "idle",
		SleepReasonIdleTimeout:           "idle-timeout",
		SleepReasonNoWakeReason:          "no-wake-reason",
		SleepReasonConfigDrift:           "config-drift",
		SleepReasonDrained:               "drained",
		SleepReasonCityStop:              "city-stop",
		SleepReasonUserHold:              "user-hold",
		SleepReasonWaitHold:              "wait-hold",
		SleepReasonRateLimit:             "rate_limit",
		SleepReasonFailedCreate:          "failed-create",
		SleepReasonProviderTerminalError: "provider-terminal-error",
		SleepReasonRuntimeMissing:        "runtime-missing",
		SleepReasonQuarantine:            "quarantine",
		SleepReasonContextChurn:          "context-churn",
		SleepReasonMaxSessionAge:         "max-session-age",
	}
	for reason, str := range want {
		if string(reason) != str {
			t.Errorf("SleepReason %q = %q, want %q", str, string(reason), str)
		}
	}
	// SleepReasonRuntimeMissing must stay pinned to the shared display-reason
	// constant so the two surfaces of the same posture never drift apart.
	if string(SleepReasonRuntimeMissing) != LifecycleReasonRuntimeMissing {
		t.Errorf("SleepReasonRuntimeMissing = %q, want %q (LifecycleReasonRuntimeMissing)",
			SleepReasonRuntimeMissing, LifecycleReasonRuntimeMissing)
	}
}

// TestSleepReasonListDivergence locks in the documented, deliberate divergence
// between the churn-suppression list (IsDeliberateSleepReason) and the
// continuation-reset list (shouldResetContinuation): the former has
// "failed-create" and lacks "runtime-missing"; the latter is the reverse.
// Merging the two lists is a bug, so this test fails if they ever converge on
// those two elements.
func TestSleepReasonListDivergence(t *testing.T) {
	// failed-create: deliberate stop (no churn) but NOT a reset-suppressor.
	if !IsDeliberateSleepReason(string(SleepReasonFailedCreate)) {
		t.Error("failed-create must be a deliberate sleep reason")
	}
	if resetSuppressed(t, SleepReasonFailedCreate) {
		t.Error("failed-create must NOT suppress continuation reset")
	}

	// runtime-missing: reset-suppressor but NOT churn-deliberate.
	if IsDeliberateSleepReason(string(SleepReasonRuntimeMissing)) {
		t.Error("runtime-missing must NOT be a deliberate sleep reason")
	}
	if !resetSuppressed(t, SleepReasonRuntimeMissing) {
		t.Error("runtime-missing must suppress continuation reset")
	}
}

// resetSuppressed reports whether shouldResetContinuation returns false (reset
// suppressed) for the given reason on an otherwise reset-eligible input.
func resetSuppressed(t *testing.T, reason SleepReason) bool {
	t.Helper()
	input := LifecycleInput{SessionKey: "sk-1"}
	return !shouldResetContinuation(BaseStateActive, input, string(reason))
}

// TestParkedSleepReason pins park evidence. A suspend is user-hold
// vocabulary (sleep_intent, or the sleep_reason its drain recorded) paired
// with a future held_until; held_until alone is a heartbeat keep-alive. A
// wait park is a non-empty wait_hold on its own. When both stand, user-hold
// is reported. A bare marker whose partner is gone is stale, not a park.
func TestParkedSleepReason(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	cases := []struct {
		name string
		info Info
		want SleepReason
	}{
		{"wait-hold intent", Info{SleepIntent: "wait-hold", WaitHold: "true"}, SleepReasonWaitHold},
		{"wait_hold marker alone", Info{WaitHold: "true"}, SleepReasonWaitHold},
		{"padded wait_hold marker", Info{WaitHold: " true "}, SleepReasonWaitHold},
		{"user-hold intent", Info{SleepIntent: "user-hold", HeldUntil: future}, SleepReasonUserHold},
		{"padded user-hold intent", Info{SleepIntent: " user-hold ", HeldUntil: future}, SleepReasonUserHold},
		{"wait-hold completed", Info{SleepIntent: "wait-hold", SleepReason: "wait-hold", WaitHold: "true"}, SleepReasonWaitHold},
		{"user-hold completed", Info{SleepIntent: "user-hold", SleepReason: "user-hold", HeldUntil: future}, SleepReasonUserHold},
		{"user-hold reason without intent", Info{SleepReason: "user-hold", HeldUntil: future}, SleepReasonUserHold},
		{"user-hold intent under idle reason", Info{SleepIntent: "user-hold", SleepReason: "idle", HeldUntil: future}, SleepReasonUserHold},
		{"suspend over wait: user-hold wins", Info{SleepIntent: "user-hold", WaitHold: "true", HeldUntil: future}, SleepReasonUserHold},
		{"suspend recorded, wait intent", Info{SleepIntent: "wait-hold", SleepReason: "user-hold", WaitHold: "true", HeldUntil: future}, SleepReasonUserHold},
		{"expired suspend over wait", Info{SleepIntent: "user-hold", WaitHold: "true", HeldUntil: past}, SleepReasonWaitHold},
		{"healed suspend over wait", Info{SleepIntent: "user-hold", WaitHold: "true"}, SleepReasonWaitHold},
		{"stale user-hold intent, live wait", Info{SleepIntent: "user-hold", SleepReason: "wait-hold", WaitHold: "true"}, SleepReasonWaitHold},
		{"wait-hold intent without wait_hold", Info{SleepIntent: "wait-hold"}, ""},
		{"wait-hold intent with heartbeat hold", Info{SleepIntent: "wait-hold", HeldUntil: future}, ""},
		{"user-hold intent with expired hold", Info{SleepIntent: "user-hold", HeldUntil: past}, ""},
		{"user-hold intent with healed hold", Info{SleepIntent: "user-hold"}, ""},
		{"user-hold intent with unparseable hold", Info{SleepIntent: "user-hold", HeldUntil: "soon"}, ""},
		{"user-hold reason with expired hold", Info{SleepReason: "user-hold", HeldUntil: past}, ""},
		{"wait-hold reason after wait released", Info{SleepReason: "wait-hold"}, ""},
		{"idle sleep", Info{SleepReason: "idle"}, ""},
		{"idle-stop-pending intent", Info{SleepIntent: "idle-stop-pending", HeldUntil: future}, ""},
		{"heartbeat hold", Info{HeldUntil: future}, ""},
		{"empty", Info{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParkedSleepReason(tc.info, now); got != tc.want {
				t.Fatalf("ParkedSleepReason(%+v) = %q, want %q", tc.info, got, tc.want)
			}
		})
	}
}

// TestWaitSleepHoldPatch pins that registering a sleeping wait never
// overwrites a standing suspend's intent, and otherwise asks for a wait-hold
// drain as the command always did.
func TestWaitSleepHoldPatch(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	cases := []struct {
		name string
		info Info
		want MetadataPatch
	}{
		{"no hold", Info{}, MetadataPatch{"wait_hold": "true", "sleep_intent": "wait-hold"}},
		{"idle probe pending", Info{SleepIntent: "idle-stop-pending"}, MetadataPatch{"wait_hold": "true", "sleep_intent": "wait-hold"}},
		{"heartbeat hold", Info{HeldUntil: future}, MetadataPatch{"wait_hold": "true", "sleep_intent": "wait-hold"}},
		{"standing suspend", Info{SleepIntent: "user-hold", HeldUntil: future}, MetadataPatch{"wait_hold": "true"}},
		{"expired suspend", Info{SleepIntent: "user-hold", HeldUntil: past}, MetadataPatch{"wait_hold": "true", "sleep_intent": "wait-hold"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WaitSleepHoldPatch(tc.info, now); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("WaitSleepHoldPatch(%+v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}

// TestReleaseWaitHoldPatch pins the wait release: wait_hold is cleared, and
// the markers the wait occupied are handed to a suspend that still stands
// instead of being blanked.
func TestReleaseWaitHoldPatch(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	cases := []struct {
		name string
		info Info
		want MetadataPatch
	}{
		{"wait only, asleep", Info{WaitHold: "true", SleepIntent: "wait-hold", SleepReason: "wait-hold"}, MetadataPatch{"wait_hold": "", "sleep_intent": "", "sleep_reason": ""}},
		{"wait only, in flight", Info{WaitHold: "true", SleepIntent: "wait-hold"}, MetadataPatch{"wait_hold": "", "sleep_intent": ""}},
		{"wait with idle-probe intent", Info{WaitHold: "true", SleepIntent: "idle-stop-pending"}, MetadataPatch{"wait_hold": "", "sleep_intent": ""}},
		{"suspend stands, recorded as suspend", Info{WaitHold: "true", SleepIntent: "user-hold", SleepReason: "user-hold", HeldUntil: future}, MetadataPatch{"wait_hold": "", "sleep_intent": "user-hold"}},
		{"suspend stands, recorded as wait", Info{WaitHold: "true", SleepIntent: "user-hold", SleepReason: "wait-hold", HeldUntil: future}, MetadataPatch{"wait_hold": "", "sleep_intent": "user-hold", "sleep_reason": "user-hold"}},
		{"expired suspend does not survive", Info{WaitHold: "true", SleepIntent: "user-hold", SleepReason: "wait-hold", HeldUntil: past}, MetadataPatch{"wait_hold": "", "sleep_intent": "", "sleep_reason": ""}},
		{"heartbeat hold is not a suspend", Info{WaitHold: "true", SleepIntent: "wait-hold", SleepReason: "wait-hold", HeldUntil: future}, MetadataPatch{"wait_hold": "", "sleep_intent": "", "sleep_reason": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReleaseWaitHoldPatch(tc.info, now); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ReleaseWaitHoldPatch(%+v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}

// TestDrainCompletionSleepReason pins how a completing drain picks the reason
// it records: live park evidence wins over the drain's tracked reason (a park
// that arrived after an idle drain began); a standing drain reason with no
// live evidence (a hold that expired mid-drain, a wait released mid-drain)
// completes as ordinary idle; everything else passes through.
func TestDrainCompletionSleepReason(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(time.RFC3339)
	cases := []struct {
		name        string
		info        Info
		drainReason string
		want        SleepReason
	}{
		{"ordinary idle", Info{}, "idle", SleepReasonIdle},
		{"ordinary no-wake-reason", Info{}, "no-wake-reason", SleepReasonNoWakeReason},
		{"config-drift passes through", Info{}, "config-drift", SleepReasonConfigDrift},
		{"heartbeat hold does not relabel", Info{HeldUntil: future}, "idle", SleepReasonIdle},
		{"park drain with live hold", Info{SleepIntent: "user-hold", HeldUntil: future}, "user-hold", SleepReasonUserHold},
		{"park drain with live wait", Info{SleepIntent: "wait-hold", WaitHold: "true"}, "wait-hold", SleepReasonWaitHold},
		{"late suspend over idle drain", Info{SleepIntent: "user-hold", HeldUntil: future}, "idle", SleepReasonUserHold},
		{"late wait over idle drain", Info{SleepIntent: "wait-hold", WaitHold: "true"}, "idle", SleepReasonWaitHold},
		{"hold expired, intent not yet healed", Info{SleepIntent: "user-hold"}, "user-hold", SleepReasonIdle},
		{"hold healed and intent released", Info{}, "user-hold", SleepReasonIdle},
		{"wait released mid-drain", Info{}, "wait-hold", SleepReasonIdle},
		{"suspend expired over a standing wait", Info{WaitHold: "true"}, "wait-hold", SleepReasonWaitHold},
		{"suspend expired over a wait, idle drain", Info{WaitHold: "true"}, "idle", SleepReasonWaitHold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DrainCompletionSleepReason(tc.info, tc.drainReason, now); got != tc.want {
				t.Fatalf("DrainCompletionSleepReason(%+v, %q) = %q, want %q", tc.info, tc.drainReason, got, tc.want)
			}
		})
	}
}
