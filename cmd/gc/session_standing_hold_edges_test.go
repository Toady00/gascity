package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// These tests extend session_standing_hold_test.go to the edges of the
// standing-hold contract (gastownhall/gascity#5561): a pool park that holds no
// claim the close gate can see, a park that lands while an ordinary idle drain
// is already in flight, and a finite hold that expires before or after its
// drain completes. They drive the real reconciler through the reconciler-owned
// drain (deferred ack, stop-pending, async stop, finalize) or the drain
// timeout, then through the stranded-repair confirmation window.
//
// intentionalParkFixture is a pool worker that holds an open claim and has
// deliberately parked itself — `gc session wait --sleep` (wait_hold +
// sleep_intent=wait-hold) or `gc session suspend` (held_until +
// sleep_intent=user-hold + state=suspended). The runtime is alive when the
// fixture is built; the reconciler is expected to drain it to sleep through
// the reconciler-owned drain-ack path and then leave the parked bead and its
// claim alone until the park is released.
type intentionalParkFixture struct {
	env     *reconcilerTestEnv
	dops    drainOps
	rec     *events.Fake
	session beads.Bead
	work    beads.Bead
	blocker beads.Bead
	// claimReady presents the claim as unblocked awake demand (the blocker
	// closed, or the claim never had one): an in_progress row without bd's
	// IsBlocked verdict, an open row flagged ready. Off by default so the
	// claim is dependency-blocked and carries no wake demand of its own.
	claimReady bool
	// poolDesired is the controller's desired count for the worker template
	// (1 by default). Zero models a pool with no scale demand, which is what
	// lets an unparked seat with only blocked work begin an ordinary idle drain.
	poolDesired int
	// holdDuration is the `gc session suspend` hold length; zero uses the
	// command's indefinite default. A finite value models a hold that can
	// expire while the park is in flight or after it completed.
	holdDuration time.Duration
}

const intentionalParkWorker = "worker-1"

// newIntentionalParkFixture builds the fixture. intent selects the park
// mechanism ("wait-hold", "user-hold", or "" for an unparked seat); workStatus
// is the raw status of the claimed work bead ("in_progress", "open", or
// "blocked"). In every case the work bead carries a blocking dependency on an
// open sibling so it is not ready.
func newIntentionalParkFixture(t *testing.T, intent, workStatus string) *intentionalParkFixture {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
	}
	rec := events.NewFake()
	env.rec = rec
	env.addDesired(intentionalParkWorker, "worker", true)
	session := env.createSessionBead(intentionalParkWorker, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pool_managed":   "true",
		"session_origin": "ephemeral",
		// Woke well before this tick so the dead-session classifier does not
		// treat the later stop as a rapid crash.
		"last_woke_at": env.clk.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	})
	f := &intentionalParkFixture{
		env:         env,
		dops:        newDrainOps(env.sp),
		rec:         rec,
		session:     session,
		poolDesired: 1,
	}
	if intent != "" {
		f.applyPark(t, intent)
	}
	if err := env.sp.SetMeta(intentionalParkWorker, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	if err := env.sp.SetMeta(intentionalParkWorker, "GC_INSTANCE_TOKEN", "test-token"); err != nil {
		t.Fatalf("SetMeta(GC_INSTANCE_TOKEN): %v", err)
	}

	blocker, err := env.store.Create(beads.Bead{Title: "prerequisite", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	work, err := env.store.Create(beads.Bead{Title: "claimed work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	assignee := session.ID
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &workStatus, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}
	if err := env.store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(work blocks-on blocker): %v", err)
	}
	work, err = env.store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work): %v", err)
	}
	f.work = work
	f.blocker = blocker
	return f
}

// applyPark writes the park exactly as the CLI does: `gc session wait --sleep`
// (cmd_wait.go doSessionWait) stamps wait_hold + sleep_intent=wait-hold;
// `gc session suspend` (cmd_session.go cmdSessionSuspend, managed path) stamps
// held_until + sleep_intent=user-hold + state=suspended.
func (f *intentionalParkFixture) applyPark(t *testing.T, intent string) {
	t.Helper()
	meta := map[string]string{}
	switch intent {
	case "wait-hold":
		meta["wait_hold"] = "true"
		meta["sleep_intent"] = "wait-hold"
	case "user-hold":
		hold := f.holdDuration
		if hold == 0 {
			hold = indefiniteHoldDuration
		}
		meta["held_until"] = f.env.clk.Now().Add(hold).UTC().Format(time.RFC3339)
		meta["sleep_intent"] = "user-hold"
		meta["state"] = "suspended"
	default:
		t.Fatalf("unknown park intent %q", intent)
	}
	f.env.setSessionMetadata(&f.session, meta)
}

// tick runs one reconciler pass over the CURRENT persisted session bead with
// the given ready-wait set, threading the claimed work as the assigned-work
// snapshot exactly as the controller does.
func (f *intentionalParkFixture) tick(t *testing.T, readyWaitSet map[string]bool) int {
	t.Helper()
	got, err := f.env.store.Get(f.session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", f.session.ID, err)
	}
	work, err := f.env.store.Get(f.work.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", f.work.ID, err)
	}
	if got.Status == "closed" {
		// Closed beads never reach the reconciler input.
		return 0
	}
	// Mirror the controller's assigned-work snapshot: an in_progress claim
	// behind an open blocking dependency carries bd's denormalized IsBlocked
	// verdict (compute_awake_bridge.go), and an open assignment is not ready
	// (no readyAssignedFlags entry), so neither is awake demand on its own.
	blocked := !f.claimReady
	if work.Status == "in_progress" {
		work.IsBlocked = &blocked
	}
	opts := append([]startExecutionOption(nil), f.env.startOptions...)
	opts = append(opts, withReadyAssignedFlags([]bool{f.claimReady}))
	return reconcileSessionBeads(
		context.Background(), []beads.Bead{got}, f.env.desiredState,
		configuredSessionNames(f.env.cfg, "", f.env.store), f.env.cfg, f.env.sp, f.env.store,
		f.dops, []beads.Bead{work}, readyWaitSet, f.env.dt, map[string]int{"worker": f.poolDesired}, false, nil, "",
		nil, f.env.clk, f.env.rec, 0, 0, &f.env.stdout, &f.env.stderr, opts...,
	)
}

func (f *intentionalParkFixture) sessionBead(t *testing.T) beads.Bead {
	t.Helper()
	got, err := f.env.store.Get(f.session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", f.session.ID, err)
	}
	return got
}

func (f *intentionalParkFixture) workBead(t *testing.T) beads.Bead {
	t.Helper()
	got, err := f.env.store.Get(f.work.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", f.work.ID, err)
	}
	return got
}

// drainToSleep walks the fixture through the reconciler-owned drain: tick 1
// begins the drain in the awake scan and publishes the deferred GC_DRAIN_ACK
// in the same tick's Phase 2 drain advance; tick 2 consumes the ack
// (drain-ack stop-pending + async provider stop); tick 3 finalizes the
// stopped session; tick 4 is a settled no-op. It returns the persisted session
// bead after the last tick.
func (f *intentionalParkFixture) drainToSleep(t *testing.T, intent string) beads.Bead {
	t.Helper()
	if woken := f.tick(t, nil); woken != 0 {
		t.Fatalf("tick 1 woken = %d, want 0; stderr=%s", woken, f.env.stderr.String())
	}
	ds := f.env.dt.get(f.session.ID)
	if ds == nil {
		t.Fatalf("tick 1: expected the park intent to begin a drain; stderr=%s", f.env.stderr.String())
	}
	if ds.reason != intent {
		t.Fatalf("tick 1 drain reason = %q, want %q", ds.reason, intent)
	}
	if ack, _ := f.env.sp.GetMeta(intentionalParkWorker, "GC_DRAIN_ACK"); ack != "1" {
		t.Fatalf("tick 1: reconciler-owned GC_DRAIN_ACK = %q, want 1", ack)
	}
	if woken := f.tick(t, nil); woken != 0 {
		t.Fatalf("tick 2 woken = %d, want 0", woken)
	}
	waitForProviderStopped(t, f.env.sp, intentionalParkWorker)
	if woken := f.tick(t, nil); woken != 0 {
		t.Fatalf("tick 3 woken = %d, want 0", woken)
	}
	if woken := f.tick(t, nil); woken != 0 {
		t.Fatalf("tick 4 woken = %d, want 0", woken)
	}
	return f.sessionBead(t)
}

// assertRestartedOnce pins "one wake, on the parked seat, no duplicate": the
// provider recorded exactly two Start calls — the fixture's initial launch of
// the worker and the single wake after the park — and both name the parked
// worker. Counting raw calls (not the deduplicated name set) is what rules out
// a second start of the same seat.
func (f *intentionalParkFixture) assertRestartedOnce(t *testing.T) {
	t.Helper()
	var starts []string
	for _, call := range f.env.sp.Calls {
		if call.Method == "Start" {
			starts = append(starts, call.Name)
		}
	}
	if len(starts) != 2 || starts[0] != intentionalParkWorker || starts[1] != intentionalParkWorker {
		t.Fatalf("provider Start calls = %v, want exactly [%s %s] (initial launch + one wake)", starts, intentionalParkWorker, intentionalParkWorker)
	}
}

func (f *intentionalParkFixture) assertParkedWithClaim(t *testing.T, intent, workStatus string) {
	t.Helper()
	got := f.sessionBead(t)
	if got.Status == "closed" {
		t.Fatalf("parked session bead was closed: close_reason=%q metadata=%v", got.Metadata["close_reason"], got.Metadata)
	}
	if state := got.Metadata["state"]; state != "asleep" {
		t.Fatalf("state = %q, want asleep", state)
	}
	if reason := got.Metadata["sleep_reason"]; reason != intent {
		t.Errorf("sleep_reason = %q, want %q (an intentional park must not be recorded as an ordinary idle sleep)", reason, intent)
	}
	if standing := got.Metadata["sleep_intent"]; standing != intent {
		t.Errorf("sleep_intent = %q, want %q (a standing hold outlives the drain it provoked)", standing, intent)
	}
	switch intent {
	case "wait-hold":
		if got.Metadata["wait_hold"] != "true" {
			t.Errorf("wait_hold = %q, want true", got.Metadata["wait_hold"])
		}
	case "user-hold":
		if got.Metadata["held_until"] == "" {
			t.Error("held_until cleared on a suspended session")
		}
	}
	work := f.workBead(t)
	if work.Assignee != f.session.ID {
		t.Fatalf("work assignee = %q, want %q (the parked session must keep its claim)", work.Assignee, f.session.ID)
	}
	if work.Status != workStatus {
		t.Fatalf("work status = %q, want %q", work.Status, workStatus)
	}
	if f.env.sp.IsRunning(intentionalParkWorker) {
		t.Fatal("parked worker runtime must stay stopped")
	}
	for _, e := range f.rec.Events {
		switch e.Type {
		case events.SessionStranded, events.BeadDeadAssigneeReopened, events.SessionDrainAckedWithAssignedWork:
			t.Fatalf("intentional park emitted %s: %s", e.Type, e.Message)
		}
	}
}

// TestReconcileSessionBeads_IntentionalParkKeepsClaimAcrossStrandedGrace pins
// the durable-responsibility contract for an intentionally parked pool worker:
// a session that parked itself via `gc session wait --sleep` or `gc session
// suspend` while holding a claim is drained to sleep by the reconciler, and
// the sleep it lands in must be the park (sleep_reason = wait-hold /
// user-hold), not an ordinary idle sleep. An ordinary idle sleep makes the
// bead pool-slot-freeable, at which point the stranded-worker repair lane
// releases the claim and closes the bead once the confirmation grace ages —
// losing the park and the ownership the agent deliberately kept.
func TestReconcileSessionBeads_IntentionalParkKeepsClaimAcrossStrandedGrace(t *testing.T) {
	cases := []struct {
		intent     string
		workStatus string
	}{
		{"wait-hold", "in_progress"},
		{"wait-hold", "open"},
		{"user-hold", "in_progress"},
		{"user-hold", "open"},
		// A raw `blocked` claim is invisible to the open/in_progress
		// assigned-work close gate on this store, which is exactly the shape
		// that let the pool close gate retire a parked bead outright while the
		// claim stayed assigned to it. The park must hold independently of
		// whether the gate can see the claim.
		{"wait-hold", "blocked"},
		{"user-hold", "blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.intent+"/"+tc.workStatus, func(t *testing.T) {
			f := newIntentionalParkFixture(t, tc.intent, tc.workStatus)
			f.drainToSleep(t, tc.intent)
			f.assertParkedWithClaim(t, tc.intent, tc.workStatus)

			// Age past the stranded-repair confirmation window across several
			// ticks: the diagnostic marker would be stamped on the first
			// not-alive tick and the repair fires once it ages past the grace.
			for i := 0; i < 3; i++ {
				f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
				if woken := f.tick(t, nil); woken != 0 {
					t.Fatalf("grace tick %d woken = %d, want 0 (a parked session has no wake reason)", i, woken)
				}
			}
			f.assertParkedWithClaim(t, tc.intent, tc.workStatus)
		})
	}
}

// TestReconcileSessionBeads_WaitHoldParkWakesOnReadyWaitWithClaimIntact pins
// the release side of the wait-hold park: once the durable wait is ready the
// SAME session wakes (one start, on the parked bead) and still owns its claim.
func TestReconcileSessionBeads_WaitHoldParkWakesOnReadyWaitWithClaimIntact(t *testing.T) {
	f := newIntentionalParkFixture(t, "wait-hold", "in_progress")
	f.drainToSleep(t, "wait-hold")
	f.assertParkedWithClaim(t, "wait-hold", "in_progress")
	f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
	if woken := f.tick(t, nil); woken != 0 {
		t.Fatalf("grace tick woken = %d, want 0", woken)
	}
	f.assertParkedWithClaim(t, "wait-hold", "in_progress")

	// The wait becomes ready: the parked session must wake — exactly once, on
	// its own bead — with its claim intact.
	if woken := f.tick(t, map[string]bool{f.session.ID: true}); woken != 1 {
		t.Fatalf("ready-wait tick woken = %d, want 1; stderr=%s", woken, f.env.stderr.String())
	}
	if !f.env.sp.IsRunning(intentionalParkWorker) {
		t.Fatal("ready wait must wake the parked worker")
	}
	f.assertRestartedOnce(t)
	got := f.sessionBead(t)
	if got.Status == "closed" {
		t.Fatalf("woken session bead is closed: %v", got.Metadata)
	}
	work := f.workBead(t)
	if work.Assignee != f.session.ID || work.Status != "in_progress" {
		t.Fatalf("work after wake: assignee=%q status=%q, want %s/in_progress", work.Assignee, work.Status, f.session.ID)
	}
}

// TestReconcileSessionBeads_UserHoldParkWakesOnExplicitWakeWithClaimIntact pins
// the release side of the suspend park: `gc session wake` clears the hold and
// records an explicit wake request; the same session wakes with its claim.
func TestReconcileSessionBeads_UserHoldParkWakesOnExplicitWakeWithClaimIntact(t *testing.T) {
	f := newIntentionalParkFixture(t, "user-hold", "in_progress")
	f.drainToSleep(t, "user-hold")
	f.assertParkedWithClaim(t, "user-hold", "in_progress")
	f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
	if woken := f.tick(t, nil); woken != 0 {
		t.Fatalf("grace tick woken = %d, want 0", woken)
	}
	f.assertParkedWithClaim(t, "user-hold", "in_progress")

	// `gc session wake` on a parked bead: the session store clears the hold
	// blockers and records an explicit wake request.
	if _, err := sessionFrontDoor(f.env.store).WakeSession(f.session.ID, f.env.clk.Now(), sessionpkg.WakeOpts{}); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}
	if woken := f.tick(t, nil); woken != 1 {
		t.Fatalf("explicit-wake tick woken = %d, want 1; stderr=%s", woken, f.env.stderr.String())
	}
	if !f.env.sp.IsRunning(intentionalParkWorker) {
		t.Fatal("explicit wake must restart the suspended worker")
	}
	f.assertRestartedOnce(t)
	work := f.workBead(t)
	if work.Assignee != f.session.ID || work.Status != "in_progress" {
		t.Fatalf("work after wake: assignee=%q status=%q, want %s/in_progress", work.Assignee, work.Status, f.session.ID)
	}
}

// TestReconcileSessionBeads_OrdinaryIdleDrainAckStillReleasesStrandedClaim is
// the control: a pool worker that drain-acks with NO park intent while holding
// a claim lands in the ordinary idle sleep, and the stranded-repair lane still
// releases the claim and frees the slot once the grace ages. The park fix must
// not turn every slept-with-claim worker into an immortal slot.
func TestReconcileSessionBeads_OrdinaryIdleDrainAckStillReleasesStrandedClaim(t *testing.T) {
	f := newIntentionalParkFixture(t, "wait-hold", "in_progress")
	// Strip the park: this is a plain worker with an agent-sourced drain-ack.
	f.env.setSessionMetadata(&f.session, map[string]string{"wait_hold": "", "sleep_intent": ""})
	if err := f.dops.setDrainAck(intentionalParkWorker); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	if woken := f.tick(t, nil); woken != 0 {
		t.Fatalf("drain-ack tick woken = %d, want 0", woken)
	}
	waitForProviderStopped(t, f.env.sp, intentionalParkWorker)
	if woken := f.tick(t, nil); woken != 0 {
		t.Fatalf("finalize tick woken = %d, want 0", woken)
	}
	got := f.sessionBead(t)
	if got.Status == "closed" {
		t.Fatalf("session closed before the stranded confirmation window: %v", got.Metadata)
	}
	if got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("state=%q sleep_reason=%q, want asleep/idle for an ordinary drain-ack with a claim", got.Metadata["state"], got.Metadata["sleep_reason"])
	}
	for i := 0; i < 3; i++ {
		f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
		f.tick(t, nil)
	}
	got = f.sessionBead(t)
	if got.Status != "closed" {
		t.Fatalf("ordinary idle-slept worker with a stranded claim must be repaired and closed after the grace, got status=%q metadata=%v; stderr=%s", got.Status, got.Metadata, f.env.stderr.String())
	}
	work := f.workBead(t)
	if work.Assignee != "" || work.Status != "open" {
		t.Fatalf("stranded claim must be released: assignee=%q status=%q", work.Assignee, work.Status)
	}
}

// TestReconcileSessionBeads_IntentionalParkWithoutClaimKeepsBead pins the
// pool close gate for a park that holds no work: a pool-managed session is
// normally closed on drain-ack when nothing is assigned to it, but a parked
// session owns its wait/hold, and closing the bead would orphan the wake
// condition. The park keeps its bead and records the park reason.
func TestReconcileSessionBeads_IntentionalParkWithoutClaimKeepsBead(t *testing.T) {
	for _, intent := range []string{"wait-hold", "user-hold"} {
		t.Run(intent, func(t *testing.T) {
			f := newIntentionalParkFixture(t, intent, "open")
			// Release the claim so the session holds nothing.
			unassigned := ""
			if err := f.env.store.Update(f.work.ID, beads.UpdateOpts{Assignee: &unassigned}); err != nil {
				t.Fatalf("Update(work): %v", err)
			}
			got := f.drainToSleep(t, intent)
			if got.Status == "closed" {
				t.Fatalf("parked session without a claim was closed: close_reason=%q", got.Metadata["close_reason"])
			}
			if got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != intent {
				t.Fatalf("state=%q sleep_reason=%q, want asleep/%s", got.Metadata["state"], got.Metadata["sleep_reason"], intent)
			}
			for i := 0; i < 3; i++ {
				f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
				if woken := f.tick(t, nil); woken != 0 {
					t.Fatalf("grace tick %d woken = %d, want 0", i, woken)
				}
			}
			if got := f.sessionBead(t); got.Status == "closed" {
				t.Fatalf("parked session without a claim was closed after the grace: close_reason=%q", got.Metadata["close_reason"])
			}
		})
	}
}

// TestReconcileSessionBeads_IntentionalParkWithReadyClaimStaysDown pins the
// park against the assigned-work wake paths when the claim itself is awake
// demand (an unblocked in_progress row, or a ready open row): the wait hold
// suppresses demand-driven wake, and the suspend hold must not be mistaken
// for a `gc runtime heartbeat` keep-alive hold by the heartbeat crash-recovery
// override after its drain completed. A parked session with ready work stays
// down until its own release (ready wait / explicit wake).
func TestReconcileSessionBeads_IntentionalParkWithReadyClaimStaysDown(t *testing.T) {
	cases := []struct {
		intent     string
		workStatus string
	}{
		{"wait-hold", "in_progress"},
		{"wait-hold", "open"},
		{"user-hold", "in_progress"},
		{"user-hold", "open"},
	}
	for _, tc := range cases {
		t.Run(tc.intent+"/"+tc.workStatus, func(t *testing.T) {
			f := newIntentionalParkFixture(t, tc.intent, tc.workStatus)
			f.claimReady = true
			f.drainToSleep(t, tc.intent)
			f.assertParkedWithClaim(t, tc.intent, tc.workStatus)
			for i := 0; i < 3; i++ {
				f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
				if woken := f.tick(t, nil); woken != 0 {
					t.Fatalf("tick %d woken = %d, want 0 (a parked session must not be respawned for its own claim); stderr=%s", i, woken, f.env.stderr.String())
				}
				if f.env.sp.IsRunning(intentionalParkWorker) {
					t.Fatalf("tick %d: parked worker was restarted", i)
				}
			}
			f.assertParkedWithClaim(t, tc.intent, tc.workStatus)
		})
	}
}

// TestReconcileSessionBeads_IntentionalParkViaDrainTimeoutStaysDown covers the
// other drain-completion writer: a park whose drain is force-stopped at
// defaultDrainTimeout completes through completeDrain and must equally stay
// parked with its ready claim.
func TestReconcileSessionBeads_IntentionalParkViaDrainTimeoutStaysDown(t *testing.T) {
	for _, intent := range []string{"wait-hold", "user-hold"} {
		t.Run(intent, func(t *testing.T) {
			f := newIntentionalParkFixture(t, intent, "in_progress")
			f.claimReady = true
			// No drain ops: the deferred reconciler ack cannot be consumed, so
			// the drain runs to its deadline and completes through completeDrain.
			f.dops = nil
			if woken := f.tick(t, nil); woken != 0 {
				t.Fatalf("tick 1 woken = %d, want 0", woken)
			}
			if ds := f.env.dt.get(f.session.ID); ds == nil || ds.reason != intent {
				t.Fatalf("tick 1 drain = %+v, want reason %q", ds, intent)
			}
			// Cross the drain deadline: tick 2 force-stops and completes the drain.
			f.env.clk.Time = f.env.clk.Now().Add(defaultDrainTimeout + time.Minute)
			if woken := f.tick(t, nil); woken != 0 {
				t.Fatalf("tick 2 woken = %d, want 0", woken)
			}
			if f.env.sp.IsRunning(intentionalParkWorker) {
				t.Fatal("drain timeout must stop the parked worker")
			}
			f.assertParkedWithClaim(t, intent, "in_progress")
			for i := 0; i < 3; i++ {
				f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
				if woken := f.tick(t, nil); woken != 0 {
					t.Fatalf("tick %d woken = %d, want 0; stderr=%s", i, woken, f.env.stderr.String())
				}
			}
			f.assertParkedWithClaim(t, intent, "in_progress")
		})
	}
}

// completeInFlightDrain finishes a drain that tick 1 already began. how selects
// the writer: "ack" consumes the deferred reconciler ack (tick 2 stop-pending,
// tick 3 finalizeDrainAckStoppedSession); "timeout" removes the drain ops so
// the drain runs to defaultDrainTimeout and completes through completeDrain.
func (f *intentionalParkFixture) completeInFlightDrain(t *testing.T, how string) {
	t.Helper()
	switch how {
	case "ack":
		if ack, _ := f.env.sp.GetMeta(intentionalParkWorker, "GC_DRAIN_ACK"); ack != "1" {
			t.Fatalf("reconciler-owned GC_DRAIN_ACK = %q, want 1 before completion", ack)
		}
		if woken := f.tick(t, nil); woken != 0 {
			t.Fatalf("stop-pending tick woken = %d, want 0", woken)
		}
		waitForProviderStopped(t, f.env.sp, intentionalParkWorker)
		if woken := f.tick(t, nil); woken != 0 {
			t.Fatalf("finalize tick woken = %d, want 0", woken)
		}
	case "timeout":
		f.dops = nil
		f.env.clk.Time = f.env.clk.Now().Add(defaultDrainTimeout + time.Minute)
		if woken := f.tick(t, nil); woken != 0 {
			t.Fatalf("timeout tick woken = %d, want 0", woken)
		}
		if f.env.sp.IsRunning(intentionalParkWorker) {
			t.Fatal("drain timeout must stop the worker")
		}
	default:
		t.Fatalf("unknown completion %q", how)
	}
}

// beginIdleDrain runs tick 1 on an UNPARKED seat whose only assigned work is
// blocked and whose pool has no scale demand, under the idle probe's own
// idle-stop-pending authorization, so the awake scan begins an ordinary
// "idle" drain. Returns after asserting the drain is tracked as idle.
func (f *intentionalParkFixture) beginIdleDrain(t *testing.T) {
	t.Helper()
	f.poolDesired = 0
	f.env.setSessionMetadata(&f.session, map[string]string{"sleep_intent": "idle-stop-pending"})
	if woken := f.tick(t, nil); woken != 0 {
		t.Fatalf("idle-drain tick woken = %d, want 0; stderr=%s", woken, f.env.stderr.String())
	}
	ds := f.env.dt.get(f.session.ID)
	if ds == nil || ds.reason != "idle" {
		t.Fatalf("expected an idle drain to begin, got %+v; stdout=%s", ds, f.env.stdout.String())
	}
}

// TestReconcileSessionBeads_ParkArrivingDuringIdleDrainCompletesAsPark pins the
// late-park transition: an ordinary idle drain is already tracked (reason
// "idle") when `gc session wait --sleep` / `gc session suspend` lands.
// beginSessionDrainInfo no-ops on an existing drain, so the drain keeps its
// initial reason; BOTH completion writers must nevertheless complete the
// drain as the park the session now carries, read from live park evidence
// (sleep_intent paired with wait_hold / a future held_until), not from the
// reason the drain happened to start with. After completion the blocker
// closes (the claim becomes ready demand): the park must still hold — in
// particular a completed suspend must not be revived by the heartbeat
// crash-recovery override or reaped as a freeable idle slot.
func TestReconcileSessionBeads_ParkArrivingDuringIdleDrainCompletesAsPark(t *testing.T) {
	for _, intent := range []string{"wait-hold", "user-hold"} {
		for _, how := range []string{"ack", "timeout"} {
			t.Run(intent+"/"+how, func(t *testing.T) {
				f := newIntentionalParkFixture(t, "", "in_progress")
				f.beginIdleDrain(t)
				f.applyPark(t, intent)
				f.completeInFlightDrain(t, how)
				f.assertParkedWithClaim(t, intent, "in_progress")

				f.claimReady = true
				for i := 0; i < 3; i++ {
					f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
					if woken := f.tick(t, nil); woken != 0 {
						t.Fatalf("tick %d woken = %d, want 0 (late park must hold against ready demand); stderr=%s", i, woken, f.env.stderr.String())
					}
				}
				f.assertParkedWithClaim(t, intent, "in_progress")
			})
		}
	}
}

// TestReconcileSessionBeads_FiniteHoldExpiringDuringDrainCompletesAsOrdinarySleep
// pins the expiry-before-finalization transition: a finite suspend hold
// (`held_until` in the near future) drains the seat, and the hold expires
// while the drain is still in flight. Phase 0a heals the expired timer
// (ClearExpiredHoldPatch) before either completion writer runs, so at
// completion there is no live hold — the stale sleep_intent alone is not park
// evidence. The drain must complete as an ORDINARY sleep: the bare user-hold
// reason must not be stamped onto a seat nothing holds (which would make a
// pool slot non-freeable forever), the stale intent must be gone, and the
// seat must behave exactly like the ordinary controls afterwards — a claim
// with demand wakes it, a stranded claim is repaired after the grace, and an
// unassigned pool seat is retired.
func TestReconcileSessionBeads_FiniteHoldExpiringDuringDrainCompletesAsOrdinarySleep(t *testing.T) {
	const hold = 2 * time.Minute
	assertNoStaleHold := func(t *testing.T, f *intentionalParkFixture) {
		t.Helper()
		got := f.sessionBead(t)
		if got.Metadata["held_until"] != "" {
			t.Errorf("held_until = %q, want cleared after expiry", got.Metadata["held_until"])
		}
		if got.Metadata["sleep_intent"] != "" {
			t.Errorf("sleep_intent = %q, want cleared once its hold expired", got.Metadata["sleep_intent"])
		}
		if got.Metadata["sleep_reason"] == "user-hold" {
			t.Errorf("sleep_reason = user-hold with no live hold: the seat would never be freeable")
		}
	}
	beginFiniteHoldDrain := func(t *testing.T, f *intentionalParkFixture) {
		t.Helper()
		// No pool scale demand: once the hold is gone, only the seat's own
		// claim (if any) may decide whether it wakes.
		f.poolDesired = 0
		f.holdDuration = hold
		f.applyPark(t, "user-hold")
		if woken := f.tick(t, nil); woken != 0 {
			t.Fatalf("tick 1 woken = %d, want 0", woken)
		}
		if ds := f.env.dt.get(f.session.ID); ds == nil || ds.reason != "user-hold" {
			t.Fatalf("tick 1 drain = %+v, want user-hold", ds)
		}
		// The hold expires while the drain is in flight (inside the drain
		// deadline, so the ack path still completes normally).
		f.env.clk.Time = f.env.clk.Now().Add(hold + time.Minute)
	}
	t.Run("ready claim wakes/ack", func(t *testing.T) {
		f := newIntentionalParkFixture(t, "", "in_progress")
		f.claimReady = true
		beginFiniteHoldDrain(t, f)
		f.completeInFlightDrain(t, "ack")
		assertNoStaleHold(t, f)
		got := f.sessionBead(t)
		if got.Status == "closed" {
			t.Fatalf("seat with a claim was closed at completion: %v", got.Metadata)
		}
		// Nothing holds the seat and its claim is demand: the next tick must
		// wake it — the expired hold cannot suppress a legitimate wake.
		if woken := f.tick(t, nil); woken != 1 {
			t.Fatalf("post-expiry tick woken = %d, want 1; metadata=%v stderr=%s", woken, f.sessionBead(t).Metadata, f.env.stderr.String())
		}
		f.assertRestartedOnce(t)
		if work := f.workBead(t); work.Assignee != f.session.ID {
			t.Fatalf("claim moved on wake: assignee=%q", work.Assignee)
		}
	})
	t.Run("ready claim keeps the live seat/timeout", func(t *testing.T) {
		// On the timeout arm the seat is still alive when the hold expires,
		// so its ready claim is wake demand for a LIVE session: the
		// (cancelable) user-hold drain is canceled and the seat keeps running
		// with its claim — the hold's expiry releases the seat, it does not
		// stop it. The stale intent must not survive to re-drain it.
		f := newIntentionalParkFixture(t, "", "in_progress")
		f.claimReady = true
		beginFiniteHoldDrain(t, f)
		f.dops = nil
		f.env.clk.Time = f.env.clk.Now().Add(defaultDrainTimeout + time.Minute)
		if woken := f.tick(t, nil); woken != 0 {
			t.Fatalf("timeout tick woken = %d, want 0 (the seat is already alive)", woken)
		}
		if !f.env.sp.IsRunning(intentionalParkWorker) {
			t.Fatal("expired hold with ready demand must keep the live seat running")
		}
		if ds := f.env.dt.get(f.session.ID); ds != nil {
			t.Fatalf("drain still tracked after the hold expired with demand: %+v", ds)
		}
		assertNoStaleHold(t, f)
		if work := f.workBead(t); work.Assignee != f.session.ID {
			t.Fatalf("claim moved: assignee=%q", work.Assignee)
		}
	})
	for _, how := range []string{"ack", "timeout"} {
		t.Run("blocked claim is repaired like an ordinary idle seat/"+how, func(t *testing.T) {
			f := newIntentionalParkFixture(t, "", "in_progress")
			beginFiniteHoldDrain(t, f)
			f.completeInFlightDrain(t, how)
			assertNoStaleHold(t, f)
			got := f.sessionBead(t)
			if got.Status == "closed" || got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != "idle" {
				t.Fatalf("status=%q state=%q sleep_reason=%q, want open asleep/idle (ordinary sleep once the hold expired)", got.Status, got.Metadata["state"], got.Metadata["sleep_reason"])
			}
			for i := 0; i < 3; i++ {
				f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
				f.tick(t, nil)
			}
			if got := f.sessionBead(t); got.Status != "closed" {
				t.Fatalf("expired-hold seat with a stranded claim must be repaired like an ordinary idle seat, got status=%q metadata=%v", got.Status, got.Metadata)
			}
			if work := f.workBead(t); work.Assignee != "" || work.Status != "open" {
				t.Fatalf("stranded claim must be released: assignee=%q status=%q", work.Assignee, work.Status)
			}
		})
		t.Run("no claim retires the pool seat/"+how, func(t *testing.T) {
			f := newIntentionalParkFixture(t, "", "open")
			unassigned := ""
			if err := f.env.store.Update(f.work.ID, beads.UpdateOpts{Assignee: &unassigned}); err != nil {
				t.Fatalf("Update(work): %v", err)
			}
			beginFiniteHoldDrain(t, f)
			f.completeInFlightDrain(t, how)
			assertNoStaleHold(t, f)
			for i := 0; i < 3; i++ {
				f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
				f.tick(t, nil)
			}
			if got := f.sessionBead(t); got.Status != "closed" {
				t.Fatalf("unassigned pool seat whose hold expired must be retired, got status=%q metadata=%v", got.Status, got.Metadata)
			}
		})
	}
}

// TestReconcileSessionBeads_FiniteHoldExpiringAfterParkReleasesSeat is the
// post-completion twin: the park completes as user-hold while the hold is
// live, then the hold expires. The expired-timer heal releases the park
// (held_until, the standing user-hold intent, and the user-hold reason),
// after which the seat is an ordinary asleep seat again: a ready claim wakes
// it, and an unassigned seat carries no park marker that could keep its slot.
func TestReconcileSessionBeads_FiniteHoldExpiringAfterParkReleasesSeat(t *testing.T) {
	const hold = 30 * time.Minute
	t.Run("ready claim wakes", func(t *testing.T) {
		f := newIntentionalParkFixture(t, "", "in_progress")
		f.holdDuration = hold
		f.applyPark(t, "user-hold")
		f.claimReady = true
		f.drainToSleep(t, "user-hold")
		f.assertParkedWithClaim(t, "user-hold", "in_progress")
		if woken := f.tick(t, nil); woken != 0 {
			t.Fatalf("parked tick woken = %d, want 0 while the hold is live", woken)
		}
		f.env.clk.Time = f.env.clk.Now().Add(hold + time.Minute)
		if woken := f.tick(t, nil); woken != 1 {
			t.Fatalf("post-expiry tick woken = %d, want 1; metadata=%v stderr=%s", woken, f.sessionBead(t).Metadata, f.env.stderr.String())
		}
		f.assertRestartedOnce(t)
		got := f.sessionBead(t)
		if got.Metadata["held_until"] != "" || got.Metadata["sleep_intent"] != "" {
			t.Fatalf("expired hold left markers: held_until=%q sleep_intent=%q", got.Metadata["held_until"], got.Metadata["sleep_intent"])
		}
		if work := f.workBead(t); work.Assignee != f.session.ID {
			t.Fatalf("claim moved on wake: assignee=%q", work.Assignee)
		}
	})
	t.Run("no claim releases every park marker", func(t *testing.T) {
		f := newIntentionalParkFixture(t, "", "open")
		f.holdDuration = hold
		f.applyPark(t, "user-hold")
		unassigned := ""
		if err := f.env.store.Update(f.work.ID, beads.UpdateOpts{Assignee: &unassigned}); err != nil {
			t.Fatalf("Update(work): %v", err)
		}
		got := f.drainToSleep(t, "user-hold")
		if got.Status == "closed" || got.Metadata["sleep_reason"] != "user-hold" || got.Metadata["sleep_intent"] != "user-hold" {
			t.Fatalf("park did not complete as user-hold: status=%q sleep_reason=%q sleep_intent=%q", got.Status, got.Metadata["sleep_reason"], got.Metadata["sleep_intent"])
		}
		f.env.clk.Time = f.env.clk.Now().Add(hold + time.Minute)
		for i := 0; i < 2; i++ {
			if woken := f.tick(t, nil); woken != 0 {
				t.Fatalf("tick %d woken = %d, want 0 (no demand)", i, woken)
			}
		}
		// Whether an asleep pool seat with an empty sleep_reason is then
		// retired is the pool-slot freeable rule's call, not the park's; what
		// the park owns is that nothing it wrote survives its own expiry.
		got = f.sessionBead(t)
		for _, key := range []string{"held_until", "sleep_intent", "sleep_reason"} {
			if got.Metadata[key] != "" {
				t.Errorf("%s = %q after the hold expired, want cleared", key, got.Metadata[key])
			}
		}
	})
}

// Overlapping holds. A session can be under a wait park and a suspend park at
// once: `gc session suspend` on a wait-parked seat stamps held_until +
// sleep_intent=user-hold and leaves wait_hold standing, and a wait registered
// with --sleep while a suspend stands adds wait_hold. The two parks are
// independent — each is released by its own act (the wait resolving or
// failing; the hold expiring or an explicit wake) — so releasing one must
// leave the seat parked under the other, with that park's vocabulary in both
// sleep_intent and the recorded sleep_reason. Otherwise the seat reads as an
// ordinary idle sleep (freeable, stranded-repaired) or, for a suspend that
// lost its intent, as a heartbeat keep-alive the crash-recovery override
// respawns onto its claim.

const overlapHold = 2 * time.Minute

// addFiniteSuspend lands `gc session suspend` with a finite hold on the
// fixture's current state (wait_hold, if any, is left standing).
func (f *intentionalParkFixture) addFiniteSuspend(t *testing.T) {
	t.Helper()
	f.holdDuration = overlapHold
	f.applyPark(t, "user-hold")
}

func (f *intentionalParkFixture) assertWaitParkSurvives(t *testing.T, workStatus string) {
	t.Helper()
	got := f.sessionBead(t)
	if got.Metadata["held_until"] != "" {
		t.Fatalf("held_until = %q, want the expired suspend healed", got.Metadata["held_until"])
	}
	f.assertParkedWithClaim(t, "wait-hold", workStatus)
	if info := f.env.sessionInfo(f.session.ID); isPoolSessionSlotFreeableInfo(info) {
		t.Fatalf("wait-parked pool seat reads as freeable (state=%q sleep_reason=%q)", info.MetadataState, info.SleepReason)
	}
	for i := 0; i < 3; i++ {
		f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
		if woken := f.tick(t, nil); woken != 0 {
			t.Fatalf("grace tick %d woken = %d, want 0 (the wait is unresolved); stderr=%s", i, woken, f.env.stderr.String())
		}
	}
	f.assertParkedWithClaim(t, "wait-hold", workStatus)
}

// TestReconcileSessionBeads_SuspendExpiringOverWaitParkMidDrainKeepsWaitPark:
// a wait park's drain is in flight (tracked reason wait-hold) when a finite
// suspend lands and overwrites sleep_intent; the suspend then expires before
// either completion writer runs, so this tick's timer heal clears held_until
// and the user-hold intent. The wait is still unresolved (wait_hold stands),
// so the drain must complete as the wait park — on both writers, whether or
// not the close gate can see the claim.
func TestReconcileSessionBeads_SuspendExpiringOverWaitParkMidDrainKeepsWaitPark(t *testing.T) {
	for _, workStatus := range []string{"in_progress", "blocked"} {
		for _, how := range []string{"ack", "timeout"} {
			t.Run(workStatus+"/"+how, func(t *testing.T) {
				f := newIntentionalParkFixture(t, "wait-hold", workStatus)
				if woken := f.tick(t, nil); woken != 0 {
					t.Fatalf("tick 1 woken = %d, want 0", woken)
				}
				if ds := f.env.dt.get(f.session.ID); ds == nil || ds.reason != "wait-hold" {
					t.Fatalf("tick 1 drain = %+v, want wait-hold", ds)
				}
				f.addFiniteSuspend(t)
				f.env.clk.Time = f.env.clk.Now().Add(overlapHold + time.Minute)
				f.completeInFlightDrain(t, how)
				f.assertWaitParkSurvives(t, workStatus)
			})
		}
	}
}

// TestReconcileSessionBeads_SuspendExpiringAfterOverlappingParkRestoresWaitPark:
// both parks are live when the drain completes; the suspend expires AFTER
// completion, so no completion writer runs again. The timer heal that
// releases the suspend must itself leave the seat recorded as the wait park.
func TestReconcileSessionBeads_SuspendExpiringAfterOverlappingParkRestoresWaitPark(t *testing.T) {
	for _, workStatus := range []string{"in_progress", "blocked"} {
		t.Run(workStatus, func(t *testing.T) {
			f := newIntentionalParkFixture(t, "wait-hold", workStatus)
			f.addFiniteSuspend(t)
			f.drainToSleep(t, "user-hold")
			got := f.sessionBead(t)
			if got.Status == "closed" || got.Metadata["wait_hold"] != "true" || got.Metadata["held_until"] == "" {
				t.Fatalf("overlapping park did not complete parked: status=%q metadata=%v", got.Status, got.Metadata)
			}
			f.env.clk.Time = f.env.clk.Now().Add(overlapHold + time.Minute)
			if woken := f.tick(t, nil); woken != 0 {
				t.Fatalf("expiry tick woken = %d, want 0 (the wait is unresolved)", woken)
			}
			f.assertWaitParkSurvives(t, workStatus)
		})
	}
}

// failPendingWaits registers a deps wait on a dependency that does not exist
// and runs the controller's real wait pass (prepareWaitWakeState), which fails
// the wait and releases the session's wait hold through
// clearSessionWaitHoldIfIdle — the same release path an expired or
// continuation-stale wait takes.
func (f *intentionalParkFixture) failPendingWaits(t *testing.T) map[string]bool {
	t.Helper()
	if _, err := f.env.store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + f.session.ID},
		Metadata: map[string]string{
			"session_id":       f.session.ID,
			"session_name":     intentionalParkWorker,
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          "gc-missing",
			"dep_mode":         "all",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	ready, err := prepareWaitWakeState(f.env.store, f.env.clk.Now())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if got := f.sessionBead(t); got.Metadata["wait_hold"] != "" {
		t.Fatalf("wait_hold = %q after the wait failed, want released", got.Metadata["wait_hold"])
	}
	return ready
}

// TestReconcileSessionBeads_WaitReleaseKeepsStandingSuspend: a seat parked
// under both a wait and a (long) suspend loses its wait — the controller's
// wait pass fails it and releases the wait hold. The suspend still stands, so
// the seat must stay parked as user-hold: sleep_intent keeps naming the
// suspend (it is the only marker that separates a suspend from a heartbeat
// hold, since both set held_until), the recorded reason stays a park, and the
// claim — ready demand — must not respawn the seat through the #3994
// heartbeat crash-recovery override.
//
// Two completion orders are covered: the suspend landed on top of the wait
// before the drain completed (recorded as user-hold), and the suspend landed
// after the wait park had already completed (recorded as wait-hold).
func TestReconcileSessionBeads_WaitReleaseKeepsStandingSuspend(t *testing.T) {
	for _, order := range []string{"suspend-before-completion", "suspend-after-completion"} {
		t.Run(order, func(t *testing.T) {
			f := newIntentionalParkFixture(t, "wait-hold", "in_progress")
			f.claimReady = true
			if order == "suspend-before-completion" {
				f.applyPark(t, "user-hold")
				f.drainToSleep(t, "user-hold")
			} else {
				f.drainToSleep(t, "wait-hold")
				f.applyPark(t, "user-hold")
				if woken := f.tick(t, nil); woken != 0 {
					t.Fatalf("suspend tick woken = %d, want 0", woken)
				}
			}
			ready := f.failPendingWaits(t)
			for i := 0; i < 3; i++ {
				f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
				if woken := f.tick(t, ready); woken != 0 {
					t.Fatalf("tick %d woken = %d, want 0 (the suspend still holds the seat); metadata=%v stderr=%s",
						i, woken, f.sessionBead(t).Metadata, f.env.stderr.String())
				}
				if f.env.sp.IsRunning(intentionalParkWorker) {
					t.Fatalf("tick %d: suspended seat was respawned after its wait was released", i)
				}
			}
			got := f.sessionBead(t)
			if got.Metadata["sleep_intent"] != "user-hold" || got.Metadata["sleep_reason"] != "user-hold" {
				t.Fatalf("sleep_intent=%q sleep_reason=%q, want user-hold/user-hold while the suspend stands",
					got.Metadata["sleep_intent"], got.Metadata["sleep_reason"])
			}
			if got.Status == "closed" {
				t.Fatalf("suspended seat was closed: %v", got.Metadata)
			}
			if work := f.workBead(t); work.Assignee != f.session.ID || work.Status != "in_progress" {
				t.Fatalf("claim moved: assignee=%q status=%q", work.Assignee, work.Status)
			}
		})
	}
}

// TestReconcileSessionBeads_WaitRegisteredDuringSuspendKeepsSuspend covers
// the other registration order through the real commands: a suspended seat
// registers `gc session wait --sleep` (doSessionWait) before its drain
// completes, and the wait is later canceled through the same release
// sequence `gc wait cancel` runs (CancelWait + clearSessionWaitHoldIfIdle).
// Registering the wait must not overwrite the suspend's sleep_intent — the
// wait's own marker is wait_hold — so after the wait is gone the seat is
// still recognizably suspended and its ready claim does not respawn it.
func TestReconcileSessionBeads_WaitRegisteredDuringSuspendKeepsSuspend(t *testing.T) {
	f := newIntentionalParkFixture(t, "user-hold", "in_progress")
	f.claimReady = true
	var stdout, stderr bytes.Buffer
	if code := doSessionWait(f.session.ID, []string{f.blocker.ID}, false, "gate", true, &stdout, &stderr, sessionWaitDeps{
		sessions:       sessionFrontDoor(f.env.store),
		dependencies:   f.env.store,
		now:            f.env.clk.Now,
		pokeController: func() error { return nil },
	}); code != 0 {
		t.Fatalf("doSessionWait = %d; stderr=%s", code, stderr.String())
	}
	got := f.sessionBead(t)
	if got.Metadata["wait_hold"] != "true" || got.Metadata["sleep_intent"] != "user-hold" {
		t.Fatalf("after wait --sleep on a suspended seat: wait_hold=%q sleep_intent=%q, want true/user-hold",
			got.Metadata["wait_hold"], got.Metadata["sleep_intent"])
	}
	f.drainToSleep(t, "user-hold")
	f.assertParkedWithClaim(t, "user-hold", "in_progress")

	sessFront := sessionFrontDoor(f.env.store)
	waits, err := sessFront.WaitsForSession(f.session.ID)
	if err != nil || len(waits) != 1 {
		t.Fatalf("WaitsForSession = %v, %v; want one wait", waits, err)
	}
	if err := sessFront.CancelWait(waits[0].ID, f.env.clk.Now(), ""); err != nil {
		t.Fatalf("CancelWait: %v", err)
	}
	if err := clearSessionWaitHoldIfIdle(sessFront, f.session.ID, f.env.clk.Now()); err != nil {
		t.Fatalf("clearSessionWaitHoldIfIdle: %v", err)
	}
	for i := 0; i < 3; i++ {
		f.env.clk.Time = f.env.clk.Now().Add(strandedRepairConfirmGrace + time.Minute)
		if woken := f.tick(t, nil); woken != 0 {
			t.Fatalf("tick %d woken = %d, want 0 (the suspend still holds the seat); metadata=%v", i, woken, f.sessionBead(t).Metadata)
		}
	}
	got = f.sessionBead(t)
	if got.Metadata["wait_hold"] != "" || got.Metadata["sleep_intent"] != "user-hold" || got.Metadata["sleep_reason"] != "user-hold" {
		t.Fatalf("after wait cancel: wait_hold=%q sleep_intent=%q sleep_reason=%q, want \"\"/user-hold/user-hold",
			got.Metadata["wait_hold"], got.Metadata["sleep_intent"], got.Metadata["sleep_reason"])
	}
}

// sessionReadFaultStore fails reads of one session bead once armed while every
// write still succeeds, so a test can prove a hold mutation that depends on
// the session's current markers refuses to write blind.
type sessionReadFaultStore struct {
	beads.Store
	sessionID string
	armed     bool
	// armOnCreate arms the fault when the next bead is created (the wait
	// bead doSessionWait registers before it writes the hold).
	armOnCreate bool
}

var errSessionReadFault = errors.New("injected session read fault")

func (s *sessionReadFaultStore) Get(id string) (beads.Bead, error) {
	if s.armed && id == s.sessionID {
		return beads.Bead{}, errSessionReadFault
	}
	return s.Store.Get(id)
}

func (s *sessionReadFaultStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err == nil && s.armOnCreate {
		s.armed = true
	}
	return created, err
}

// assertSuspendMarkersUnchanged reads the raw store (not through the fault)
// and requires the standing suspend's markers exactly as before the call.
func assertSuspendMarkersUnchanged(t *testing.T, store beads.Store, id string, want map[string]string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	for key, value := range want {
		if got.Metadata[key] != value {
			t.Errorf("%s = %q, want %q unchanged", key, got.Metadata[key], value)
		}
	}
}

// TestSessionWaitSleep_UnreadableSessionWritesNoHold: `gc session wait
// --sleep` must know whether a suspend already stands before it writes the
// wait hold. When the session cannot be read (writes would still succeed) the
// command fails without touching the session's markers, instead of writing
// the plain wait-hold patch over a standing suspend's intent.
func TestSessionWaitSleep_UnreadableSessionWritesNoHold(t *testing.T) {
	f := newIntentionalParkFixture(t, "user-hold", "in_progress")
	before := f.sessionBead(t)
	want := map[string]string{
		"sleep_intent": before.Metadata["sleep_intent"],
		"held_until":   before.Metadata["held_until"],
		"wait_hold":    "",
	}
	faulty := &sessionReadFaultStore{Store: f.env.store, sessionID: f.session.ID, armOnCreate: true}
	var stdout, stderr bytes.Buffer
	code := doSessionWait(f.session.ID, []string{f.blocker.ID}, false, "gate", true, &stdout, &stderr, sessionWaitDeps{
		sessions:       sessionFrontDoor(faulty),
		dependencies:   f.env.store,
		now:            f.env.clk.Now,
		pokeController: func() error { return nil },
	})
	if !faulty.armed {
		t.Fatal("fault never armed: doSessionWait did not register a wait before the hold")
	}
	if code == 0 {
		t.Fatalf("doSessionWait = 0 with an unreadable session; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), errSessionReadFault.Error()) {
		t.Errorf("stderr = %q, want the read error reported", stderr.String())
	}
	assertSuspendMarkersUnchanged(t, f.env.store, f.session.ID, want)
	// Control: the fault blocks reads only — the same store still accepts a
	// write to this session, so an unchanged session proves no write was made.
	if err := sessionFrontDoor(faulty).ApplyPatch(f.session.ID, sessionpkg.MetadataPatch{"fault_probe": "1"}); err != nil {
		t.Fatalf("control write through the fault store: %v", err)
	}
}

// TestClearSessionWaitHold_UnreadableSessionWritesNoRelease: releasing a wait
// park re-derives the markers from the park that survives it, so an unreadable
// session is an error and nothing is written — a blind clear would erase a
// standing suspend's intent and let the heartbeat override respawn the seat.
// The wait itself was already terminal (the caller canceled/failed it first);
// this helper does not arrange a retry of the release.
func TestClearSessionWaitHold_UnreadableSessionWritesNoRelease(t *testing.T) {
	f := newIntentionalParkFixture(t, "wait-hold", "in_progress")
	f.applyPark(t, "user-hold")
	before := f.sessionBead(t)
	want := map[string]string{
		"wait_hold":    "true",
		"sleep_intent": "user-hold",
		"held_until":   before.Metadata["held_until"],
	}
	faulty := &sessionReadFaultStore{Store: f.env.store, sessionID: f.session.ID, armed: true}
	err := clearSessionWaitHoldIfIdle(sessionFrontDoor(faulty), f.session.ID, f.env.clk.Now())
	if !errors.Is(err, errSessionReadFault) {
		t.Fatalf("clearSessionWaitHoldIfIdle error = %v, want the read fault", err)
	}
	assertSuspendMarkersUnchanged(t, f.env.store, f.session.ID, want)
	if err := sessionFrontDoor(faulty).ApplyPatch(f.session.ID, sessionpkg.MetadataPatch{"fault_probe": "1"}); err != nil {
		t.Fatalf("control write through the fault store: %v", err)
	}
}
