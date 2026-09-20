package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/storeref"
)

func graphV2Root(id, assignee string, extra map[string]string) beads.Bead {
	meta := map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.RoutedToMetadataKey:        "repo/worker",
	}
	for k, v := range extra {
		meta[k] = v
	}
	status := "open"
	if assignee != "" {
		status = "in_progress"
	}
	return beads.Bead{ID: id, Title: "root", Type: "task", Status: status, Assignee: assignee, Metadata: meta}
}

func graphStep(id, rootID, status string, extra map[string]string) beads.Bead {
	meta := map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindTask,
		beadmeta.RootBeadIDMetadataKey: rootID,
		beadmeta.RoutedToMetadataKey:   "repo/worker",
	}
	for k, v := range extra {
		meta[k] = v
	}
	return beads.Bead{ID: id, Title: "step", Type: "task", Status: status, Metadata: meta}
}

func storeFactoryFor(stores map[string]beads.Store) func(string) (beads.Store, error) {
	return func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}
}

func detailsMentioning(t *testing.T, res *doctor.CheckResult, want []string, notWant []string) {
	t.Helper()
	details := strings.Join(res.Details, "\n")
	for _, s := range want {
		if !strings.Contains(details, s) {
			t.Errorf("details missing %q:\n%s", s, details)
		}
	}
	for _, s := range notWant {
		if strings.Contains(details, s) {
			t.Errorf("details should not mention %q:\n%s", s, details)
		}
	}
}

func mustBeStamped(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if !beadmeta.IsExpandedWorkflow(b.Metadata) {
			t.Errorf("%s not stamped: %#v", id, b.Metadata)
		}
	}
}

func mustNotBeStamped(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if _, stamped := b.Metadata[beadmeta.WorkflowExpandedMetadataKey]; stamped {
			t.Errorf("%s must not be stamped: %#v", id, b.Metadata)
		}
	}
}

// TestWorkflowExpandedBackfillCheck covers the pre-#5901 repair on a city that
// relocates nothing: a graph.v2 root with persisted members but no
// gc.workflow_expanded is flagged and stamped; root-only, already-stamped,
// self-referencing, foreign-ref, v1 and non-workflow beads are left alone;
// owners and routes survive the stamp; a second Fix writes nothing.
func TestWorkflowExpandedBackfillCheck(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "repo", Path: rigDir, Prefix: "rr"},
			{Name: "paused", Path: t.TempDir(), Suspended: true},
		},
	}

	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		// Unmarked expanded root with an open step: the incident shape.
		graphV2Root("WR-open", "", nil),
		graphStep("WS-open", "WR-open", "open", nil),
		// Only member is closed: waiting on its finalizer, still ready.
		graphV2Root("WR-closed-child", "", nil),
		graphStep("WS-closed", "WR-closed-child", "closed", nil),
		// Already owned: stamped, owner preserved.
		graphV2Root("WR-owned", "seat-1", nil),
		graphStep("WS-owned", "WR-owned", "in_progress", nil),
		// Root-only launch: no members.
		graphV2Root("WR-root-only", "", nil),
		// A root naming itself is not its own member.
		graphV2Root("WR-self", "", map[string]string{beadmeta.RootBeadIDMetadataKey: "WR-self"}),
		// A member stamped for another scope is not this root's.
		graphV2Root("WR-foreign", "", map[string]string{beadmeta.RootStoreRefMetadataKey: "city:test-city"}),
		graphStep("WS-foreign", "WR-foreign", "open", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:other"}),
		// Same-scope member with matching ref counts.
		graphV2Root("WR-scoped", "", map[string]string{beadmeta.RootStoreRefMetadataKey: "city:test-city"}),
		graphStep("WS-scoped", "WR-scoped", "open", map[string]string{beadmeta.RootStoreRefMetadataKey: "city:test-city"}),
		// Already stamped.
		graphV2Root("WR-stamped", "", map[string]string{beadmeta.WorkflowExpandedMetadataKey: "true"}),
		graphStep("WS-stamped", "WR-stamped", "open", nil),
		// Closed root: not ready, not read.
		{ID: "WR-done", Title: "root", Type: "task", Status: "closed", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}},
		graphStep("WS-done", "WR-done", "closed", nil),
		// v1 kind=workflow root without the graph.v2 contract.
		{ID: "V1-root", Title: "root", Type: "molecule", Status: "open", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
		{ID: "V1-step", Title: "step", Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "V1-root"}},
		// Not a workflow root at all.
		{ID: "T-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2}},
		graphStep("T-1-step", "T-1", "open", nil),
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		graphV2Root("rr-open", "", nil),
		graphStep("rr-step", "rr-open", "open", nil),
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}

	check := newWorkflowExpandedBackfillCheck(cfg, cityDir, storeFactoryFor(stores))
	if !check.CanFix() {
		t.Fatal("CanFix() = false, want true")
	}

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	detailsMentioning(t, res,
		[]string{"WR-open", "WR-closed-child", "WR-owned", "WR-scoped", "rig:repo root rr-open"},
		[]string{"WR-root-only", "WR-self", "WR-foreign", "WR-stamped", "WR-done", "V1-root", "T-1", "skipped"})

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res2.Status, res2)
	}

	mustBeStamped(t, cityStore, "WR-open", "WR-closed-child", "WR-owned", "WR-scoped")
	mustBeStamped(t, rigStore, "rr-open")
	mustNotBeStamped(t, cityStore, "WR-root-only", "WR-self", "WR-foreign", "WR-done", "V1-root", "T-1", "WS-open")
	for _, id := range []string{"WR-open", "WR-owned"} {
		b, _ := cityStore.Get(id)
		if got := b.Metadata[beadmeta.RoutedToMetadataKey]; got != "repo/worker" {
			t.Errorf("%s gc.routed_to = %q, want repo/worker preserved", id, got)
		}
	}
	if owned, _ := cityStore.Get("WR-owned"); owned.Assignee != "seat-1" || owned.Status != "in_progress" {
		t.Errorf("WR-owned = assignee %q status %q, want seat-1/in_progress preserved", owned.Assignee, owned.Status)
	}

	// Idempotent: a second Fix writes nothing.
	counting := &countingSetMetadataStore{Store: cityStore}
	stores[cityDir] = counting
	again := newWorkflowExpandedBackfillCheck(cfg, cityDir, storeFactoryFor(stores))
	if err := again.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("second Fix: %v", err)
	}
	if counting.writes != 0 {
		t.Fatalf("second Fix performed %d metadata writes, want 0", counting.writes)
	}
}

// graphBindingTopology assembles a split-city topology: the work ledger plus a
// binding serving the graph class. The binding declares no reserved prefix, so
// every id is placed through the residence probe, binding first.
func graphBindingTopology(binding beads.Store) func(beads.Store, map[string]beads.Store) storeref.Topology {
	return func(work beads.Store, rigs map[string]beads.Store) storeref.Topology {
		return assembleResidencyTopology(&config.City{Workspace: config.Workspace{Name: "test-city"}}, work, rigs,
			[]storeref.ClassBinding{{
				Classes: []coordclass.Class{coordclass.ClassGraph},
				Leg:     storeref.Leg{Ref: storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: binding},
			}}, nil)
	}
}

// TestWorkflowExpandedBackfillCheckWritesTheBindingNotTheFrozenTwin: on a
// split city the migration leaves a frozen copy of each root in the work
// ledger. The binding's row is the authority: it is the one read, proven and
// stamped; the twin is never written, and a twin whose binding copy is already
// stamped or closed is not a target at all.
func TestWorkflowExpandedBackfillCheckWritesTheBindingNotTheFrozenTwin(t *testing.T) {
	cityDir := t.TempDir()
	ledger := &countingSetMetadataStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		// Frozen twins: unmarked in the ledger, whatever the binding says.
		graphV2Root("WR-live", "", nil),
		graphV2Root("WR-already", "", nil),
		graphV2Root("WR-finished", "", nil),
		// The ledger holds the twin's members too (migration copies subtrees);
		// they must not prove the ledger copy.
		graphStep("WS-live", "WR-live", "open", nil),
	}, nil)}
	binding := beads.NewMemStoreFrom(0, []beads.Bead{
		graphV2Root("WR-live", "", nil),
		graphStep("WS-live", "WR-live", "open", nil),
		graphV2Root("WR-already", "", map[string]string{beadmeta.WorkflowExpandedMetadataKey: "true"}),
		graphStep("WS-already", "WR-already", "open", nil),
		{ID: "WR-finished", Title: "root", Type: "task", Status: "closed", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}},
		// Binding-only root and a binding-only root-only launch.
		graphV2Root("GR-open", "", nil),
		graphStep("GS-open", "GR-open", "open", nil),
		graphV2Root("GR-root-only", "", nil),
	}, nil)
	check := newWorkflowExpandedBackfillCheck(nil, cityDir, storeFactoryFor(map[string]beads.Store{cityDir: ledger}))
	check.topology = graphBindingTopology(binding)

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	graphRef := string(storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}))
	detailsMentioning(t, res,
		[]string{graphRef + " root WR-live", graphRef + " root GR-open"},
		[]string{"city work store root", "WR-already", "WR-finished", "GR-root-only", "skipped"})
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if ledger.writes != 0 {
		t.Fatalf("Fix wrote %d time(s) to the work ledger, want 0: the frozen twin is not the authority", ledger.writes)
	}
	mustBeStamped(t, binding, "WR-live", "GR-open")
	mustNotBeStamped(t, ledger, "WR-live", "WR-already", "WR-finished")
	mustNotBeStamped(t, binding, "GR-root-only")
	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res2.Status, res2)
	}
}

// TestWorkflowExpandedBackfillCheckSurfacesReadErrors: every read that fails
// is reported and blocks Fix; none is read as clean or as root-only.
func TestWorkflowExpandedBackfillCheckSurfacesReadErrors(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir, Prefix: "rr"}}}

	t.Run("city store open failure", func(t *testing.T) {
		check := newWorkflowExpandedBackfillCheck(cfg, cityDir, func(string) (beads.Store, error) {
			return nil, errors.New("dolt unreachable")
		})
		res := check.Run(&doctor.CheckContext{})
		if res.Status != doctor.StatusWarning {
			t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
		}
		detailsMentioning(t, res, []string{"city skipped", "dolt unreachable"}, nil)
		if err := check.Fix(&doctor.CheckContext{}); err == nil || !strings.Contains(err.Error(), "dolt unreachable") {
			t.Fatalf("Fix error = %v, want the skipped store surfaced", err)
		}
	})

	t.Run("rig store open failure", func(t *testing.T) {
		cityStore := beads.NewMemStoreFrom(0, nil, nil)
		check := newWorkflowExpandedBackfillCheck(cfg, cityDir, func(path string) (beads.Store, error) {
			if path == cityDir {
				return cityStore, nil
			}
			return nil, errors.New("rig dolt unreachable")
		})
		res := check.Run(&doctor.CheckContext{})
		if res.Status != doctor.StatusWarning {
			t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
		}
		detailsMentioning(t, res, []string{"rig repo skipped", "rig dolt unreachable"}, nil)
		if err := check.Fix(&doctor.CheckContext{}); err == nil || !strings.Contains(err.Error(), "rig dolt unreachable") {
			t.Fatalf("Fix error = %v, want the skipped store surfaced", err)
		}
	})

	t.Run("graph binding refused", func(t *testing.T) {
		cityStore := beads.NewMemStoreFrom(0, []beads.Bead{graphV2Root("WR-x", "", nil), graphStep("WS-x", "WR-x", "open", nil)}, nil)
		check := newWorkflowExpandedBackfillCheck(nil, cityDir, storeFactoryFor(map[string]beads.Store{cityDir: cityStore}))
		check.topology = func(beads.Store, map[string]beads.Store) storeref.Topology {
			return refusedRelicTopology(false)
		}
		res := check.Run(&doctor.CheckContext{})
		if res.Status != doctor.StatusWarning {
			t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
		}
		detailsMentioning(t, res, []string{"graph stores skipped", "storage refused"}, []string{"WR-x has"})
		if err := check.Fix(&doctor.CheckContext{}); err == nil || !strings.Contains(err.Error(), "storage refused") {
			t.Fatalf("Fix error = %v, want the refusal surfaced", err)
		}
		mustNotBeStamped(t, cityStore, "WR-x")
	})

	t.Run("membership read failure is not root-only", func(t *testing.T) {
		faulty := &metadataListFaultStore{
			Store:    beads.NewMemStoreFrom(0, []beads.Bead{graphV2Root("WR-fault", "", nil), graphStep("WS-fault", "WR-fault", "open", nil)}, nil),
			faultKey: beadmeta.RootBeadIDMetadataKey,
			err:      errors.New("members unreadable"),
		}
		check := newWorkflowExpandedBackfillCheck(nil, cityDir, storeFactoryFor(map[string]beads.Store{cityDir: faulty}))
		res := check.Run(&doctor.CheckContext{})
		if res.Status != doctor.StatusWarning {
			t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
		}
		detailsMentioning(t, res, []string{"root WR-fault skipped", "members unreadable"}, nil)
		if err := check.Fix(&doctor.CheckContext{}); err == nil || !strings.Contains(err.Error(), "members unreadable") {
			t.Fatalf("Fix error = %v, want the skipped root surfaced", err)
		}
		mustNotBeStamped(t, faulty, "WR-fault")
	})

	t.Run("root listing failure", func(t *testing.T) {
		faulty := &metadataListFaultStore{
			Store:    beads.NewMemStoreFrom(0, nil, nil),
			faultKey: beadmeta.KindMetadataKey,
			err:      errors.New("roots unreadable"),
		}
		check := newWorkflowExpandedBackfillCheck(nil, cityDir, storeFactoryFor(map[string]beads.Store{cityDir: faulty}))
		res := check.Run(&doctor.CheckContext{})
		if res.Status != doctor.StatusWarning {
			t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
		}
		detailsMentioning(t, res, []string{"workflow roots skipped", "roots unreadable"}, nil)
		if err := check.Fix(&doctor.CheckContext{}); err == nil || !strings.Contains(err.Error(), "roots unreadable") {
			t.Fatalf("Fix error = %v, want the failed listing surfaced", err)
		}
	})
}

// TestWorkflowExpandedBackfillCheckFixSurfacesWriteFailure: a failed stamp is
// returned with the root named; nothing is reported repaired.
func TestWorkflowExpandedBackfillCheckFixSurfacesWriteFailure(t *testing.T) {
	cityDir := t.TempDir()
	store := &setMetadataFaultStore{
		Store: beads.NewMemStoreFrom(0, []beads.Bead{graphV2Root("WR-w", "", nil), graphStep("WS-w", "WR-w", "open", nil)}, nil),
		err:   errors.New("write refused"),
	}
	check := newWorkflowExpandedBackfillCheck(nil, cityDir, storeFactoryFor(map[string]beads.Store{cityDir: store}))
	err := check.Fix(&doctor.CheckContext{})
	if err == nil || !strings.Contains(err.Error(), "WR-w") || !strings.Contains(err.Error(), "write refused") {
		t.Fatalf("Fix error = %v, want the root and the write failure named", err)
	}
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusWarning {
		t.Fatalf("post-failed-fix Run status = %v, want warning (still unmarked): %#v", res.Status, res)
	}
}

// TestWorkflowExpandedBackfillCheckReadsLiveAcrossTiersIncludingClosedMembers
// pins the read shape: every list is a live read over both storage tiers, and
// the membership read includes closed rows while the root read does not.
func TestWorkflowExpandedBackfillCheckReadsLiveAcrossTiersIncludingClosedMembers(t *testing.T) {
	cityDir := t.TempDir()
	recording := &recordingListStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		graphV2Root("WR-t", "", nil),
		graphStep("WS-t", "WR-t", "closed", nil),
	}, nil)}
	check := newWorkflowExpandedBackfillCheck(nil, cityDir, storeFactoryFor(map[string]beads.Store{cityDir: recording}))
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	var rootReads, memberReads int
	for _, q := range recording.queries {
		if !q.Live || q.TierMode != beads.TierBoth {
			t.Errorf("list query is not a live both-tier read: %+v", q)
		}
		switch {
		case q.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow:
			rootReads++
			if q.IncludeClosed {
				t.Errorf("root listing must not include closed roots: %+v", q)
			}
		case q.Metadata[beadmeta.RootBeadIDMetadataKey] == "WR-t":
			memberReads++
			if !q.IncludeClosed {
				t.Errorf("membership listing must include closed members: %+v", q)
			}
		default:
			t.Errorf("unexpected list query: %+v", q)
		}
	}
	if rootReads != 1 || memberReads != 1 {
		t.Fatalf("root reads = %d, member reads = %d, want 1 and 1", rootReads, memberReads)
	}
}

// TestWorkflowExpandedBackfillCheckCleanStore confirms a store with only
// stamped or root-only roots reports OK.
func TestWorkflowExpandedBackfillCheckCleanStore(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		graphV2Root("WR-stamped", "", map[string]string{beadmeta.WorkflowExpandedMetadataKey: "true"}),
		graphStep("WS-stamped", "WR-stamped", "open", nil),
		graphV2Root("WR-root-only", "", nil),
	}, nil)
	check := newWorkflowExpandedBackfillCheck(nil, cityDir, storeFactoryFor(map[string]beads.Store{cityDir: store}))
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("Run status = %v, want OK: %#v", res.Status, res)
	}
}

type countingSetMetadataStore struct {
	beads.Store
	writes int
}

func (s *countingSetMetadataStore) SetMetadata(id, key, value string) error {
	s.writes++
	return s.Store.SetMetadata(id, key, value)
}

type setMetadataFaultStore struct {
	beads.Store
	err error
}

func (s *setMetadataFaultStore) SetMetadata(string, string, string) error { return s.err }

// metadataListFaultStore fails any List whose metadata filter carries faultKey.
type metadataListFaultStore struct {
	beads.Store
	faultKey string
	err      error
}

func (s *metadataListFaultStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if _, ok := query.Metadata[s.faultKey]; ok {
		return nil, s.err
	}
	return s.Store.List(query)
}

// recordingListStore records every List query it serves.
type recordingListStore struct {
	beads.Store
	queries []beads.ListQuery
}

func (s *recordingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	return s.Store.List(query)
}
