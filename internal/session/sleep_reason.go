package session

import (
	"strings"
	"time"
)

// SleepReason is the typed vocabulary for the sleep_reason bead-metadata
// marker. Before this type the ~fifteen sleep-reason strings were matched as
// raw literals across four independent classifier case lists (churn
// suppression, continuation reset, display reason, wake-blocker clearing) plus
// scattered writers, so a misspelled writer ("rate-limit" vs "rate_limit")
// silently escaped every classifier. Routing both the writers and the
// classifiers through these constants makes such a typo a compile error.
//
// The on-store string values are unchanged; SleepReason is a thin string alias
// so a raw metadata value converts with SleepReason(value) at the read edge.
type SleepReason string

// Sleep-reason values written to and read from the sleep_reason metadata key.
// SleepReasonRuntimeMissing shares its string with LifecycleReasonRuntimeMissing
// (the display-reason surface of the same posture); it is defined from that
// constant so the two never drift.
const (
	SleepReasonIdle                  SleepReason = "idle"
	SleepReasonIdleTimeout           SleepReason = "idle-timeout"
	SleepReasonNoWakeReason          SleepReason = "no-wake-reason"
	SleepReasonConfigDrift           SleepReason = "config-drift"
	SleepReasonDrained               SleepReason = "drained"
	SleepReasonCityStop              SleepReason = "city-stop"
	SleepReasonUserHold              SleepReason = "user-hold"
	SleepReasonWaitHold              SleepReason = "wait-hold"
	SleepReasonRateLimit             SleepReason = "rate_limit"
	SleepReasonFailedCreate          SleepReason = "failed-create"
	SleepReasonProviderTerminalError SleepReason = "provider-terminal-error"
	SleepReasonRuntimeMissing        SleepReason = SleepReason(LifecycleReasonRuntimeMissing)
	SleepReasonQuarantine            SleepReason = "quarantine"
	SleepReasonContextChurn          SleepReason = "context-churn"
	SleepReasonMaxSessionAge         SleepReason = "max-session-age"
	SleepReasonAssignedWorkExhausted SleepReason = "assigned-work-exhausted"
)

// IsDeliberateSleepReason reports whether a sleep_reason records an
// intentional stop rather than a crash, so the death must not accrue churn.
// "city-stop" mirrors the CLI's stop sleep reason.
// "provider-terminal-error" is a classified, non-retryable provider failure
// (set by markProviderTerminalError); it is suppressed here so a session
// already parked terminal cannot also accrue a spurious wake failure, making
// the invariant explicit rather than relying on last_woke_at being cleared in
// the same metadata batch.
// The reason list deliberately diverges from shouldResetContinuation's
// near-identical list (this one has "failed-create" and lacks
// "runtime-missing"): that one decides continuation reset on wake, this one
// decides churn suppression — do not merge the lists.
func IsDeliberateSleepReason(reason string) bool {
	switch SleepReason(strings.TrimSpace(reason)) {
	case SleepReasonIdle, SleepReasonIdleTimeout, SleepReasonNoWakeReason,
		SleepReasonConfigDrift, SleepReasonDrained, SleepReasonCityStop,
		SleepReasonUserHold, SleepReasonWaitHold, SleepReasonRateLimit,
		SleepReasonFailedCreate, SleepReasonProviderTerminalError:
		return true
	default:
		return false
	}
}

// StandingSleepIntent returns the canonical standing-hold intent carried by a
// raw sleep_intent marker, or "" when the marker is not a standing hold.
//
// A STANDING hold outlives the drain it provokes, because a later explicit act
// releases it: an operator suspend (`gc session suspend`, which pairs the
// intent with held_until) or a wait gate (`gc session wait --sleep`, which
// pairs it with wait_hold). Both are cleared by ClearWakeBlockersPatch on
// `gc session wake`, and wait-hold additionally by the wait's own resolution.
// Every other intent — idle-stop-pending, the ordinary idle-drain handshake —
// is bookkeeping for the drain itself and ends with it.
//
// The distinction is load-bearing on the wake side: sleep_intent is the only
// marker separating an operator's suspend hold from an agent's
// `gc runtime heartbeat` keep-alive hold, since both set held_until. The
// heartbeat crash-recovery override respawns the latter and must not touch the
// former (gastownhall/gascity#3994).
func StandingSleepIntent(intent string) SleepReason {
	switch reason := SleepReason(strings.TrimSpace(intent)); reason {
	case SleepReasonUserHold, SleepReasonWaitHold:
		return reason
	default:
		return ""
	}
}

// ParkedSleepReason reports the standing hold a session is parked under at
// now, or "" when nothing holds it.
//
// A session carries two independent parks, each read from its own live
// marker:
//
//   - a suspend (user-hold) is a future held_until PAIRED with user-hold
//     vocabulary in sleep_intent or in the sleep_reason its completed drain
//     recorded. held_until alone is not a suspend: `gc runtime heartbeat`
//     writes the same field as a keep-alive, so the vocabulary is the only
//     thing that separates the two, and an expired or unparseable hold is
//     stale however it is labeled;
//   - a wait park (wait-hold) is a non-empty wait_hold. Only the wait
//     command's own hold writes that marker and only the wait's release
//     clears it, so it is evidence on its own, whatever sleep_intent says —
//     a suspend landing on a wait-parked seat overwrites the intent but not
//     the wait.
//
// When both stand, user-hold is reported: it is the park whose identity
// depends on the vocabulary surviving, while the wait park keeps its own
// marker. Releasing either park re-derives the vocabulary from the one that
// survives (ClearExpiredHoldPatch, ReleaseWaitHoldPatch).
func ParkedSleepReason(info Info, now time.Time) SleepReason {
	if suspendHoldLive(info, now) {
		return SleepReasonUserHold
	}
	if strings.TrimSpace(info.WaitHold) != "" {
		return SleepReasonWaitHold
	}
	return ""
}

// suspendHoldLive reports whether a user-hold park stands: user-hold
// vocabulary paired with a future held_until.
func suspendHoldLive(info Info, now time.Time) bool {
	if StandingSleepIntent(info.SleepIntent) != SleepReasonUserHold &&
		StandingSleepIntent(info.SleepReason) != SleepReasonUserHold {
		return false
	}
	held, err := time.Parse(time.RFC3339, strings.TrimSpace(info.HeldUntil))
	return err == nil && now.Before(held)
}

// WaitSleepHoldPatch parks a session on its durable wait (`gc session wait
// --sleep`): wait_hold marks the wait park, and sleep_intent=wait-hold asks
// the reconciler to drain the seat. When a suspend already stands, its
// user-hold intent is left in place — the suspend is already draining or
// parking the seat, wait_hold carries the wait park on its own, and
// overwriting the intent would erase the only marker that separates the
// suspend from a heartbeat keep-alive once the wait is released.
func WaitSleepHoldPatch(info Info, now time.Time) MetadataPatch {
	patch := MetadataPatch{"wait_hold": "true"}
	if !suspendHoldLive(info, now) {
		patch["sleep_intent"] = string(SleepReasonWaitHold)
	}
	return patch
}

// ReleaseWaitHoldPatch releases a session's wait park (its wait resolved,
// failed, expired, or was canceled) and leaves any park that still stands
// named in both markers the wait may have occupied: sleep_intent names the
// surviving park (or is cleared, as before, when none survives), and a
// wait-hold sleep_reason is rewritten to the surviving park rather than
// blanked. A standing suspend therefore keeps the user-hold vocabulary that
// separates it from a heartbeat keep-alive, and a parked pool seat never
// passes through an empty, freeable sleep_reason while its suspend holds it.
func ReleaseWaitHoldPatch(info Info, now time.Time) MetadataPatch {
	after := info
	after.WaitHold = ""
	surviving := ParkedSleepReason(after, now)
	patch := MetadataPatch{
		"wait_hold":    "",
		"sleep_intent": string(surviving),
	}
	if StandingSleepIntent(info.SleepReason) == SleepReasonWaitHold {
		patch["sleep_reason"] = string(surviving)
	}
	return patch
}

// DrainCompletionSleepReason selects the sleep_reason a completed drain
// records, from the session's live park evidence at completion rather than
// the reason the drain began with. The tracked reason is not authoritative in
// either direction: a park can land while an ordinary idle drain is already
// in flight (the drain tracker keeps the first reason), and a finite hold can
// expire before its own drain completes. Live park evidence wins; a standing
// drain reason with no live evidence completes as an ordinary idle sleep, the
// same reason a drain-ack that still holds work records; every other reason
// passes through unchanged.
func DrainCompletionSleepReason(info Info, drainReason string, now time.Time) SleepReason {
	if parked := ParkedSleepReason(info, now); parked != "" {
		return parked
	}
	if StandingSleepIntent(drainReason) != "" {
		return SleepReasonIdle
	}
	return SleepReason(drainReason)
}
