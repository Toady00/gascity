package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/storeref"
)

// workflowExpandedBackfillCheck repairs graph.v2 workflow roots persisted
// before molecule.Instantiate stamped gc.workflow_expanded (#5900). Every
// fresh-work reader refuses an unassigned expanded root by that stamp
// (beadmeta.IsExpandedWorkflow, #6461); a root that was expanded into step
// beads but predates the stamp reads as root-only and stays claimable by a
// second pool seat until it closes. --fix stamps the marker only: routing and
// assignee are left as persisted, so an owner keeps its anchor.
//
// Residency is the resolver's, not this file's. Candidates come from
// Plan(Census{graph}) over the city's topology — work, the serving rigs and
// every binding serving the graph class — and each candidate id is then placed
// by Plan(ByID), so on a split city the binding's row is the one read and
// written and the migration's frozen copy in the work ledger is never touched.
//
// Expansion is proven from persisted membership in the owner's own store: at
// least one other bead there carries gc.root_bead_id naming the root and, when
// the root carries gc.root_store_ref, the same ref. Closed members count, so a
// root waiting on its finalizer is still repaired. The root must corroborate
// as a graph.v2 workflow root (gc.kind=workflow, gc.formula_contract=graph.v2),
// which keeps v1 molecule roots and hand-authored kinds out. A root with no
// members is root-only and left unmarked.
type workflowExpandedBackfillCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
	// topology builds the residency topology over the opened scope stores.
	// nil resolves through residencyTopologyForCity, the seam every one-shot
	// command answers residency from; tests hand in an assembled topology.
	topology func(work beads.Store, rigs map[string]beads.Store) storeref.Topology
}

func newWorkflowExpandedBackfillCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *workflowExpandedBackfillCheck {
	return &workflowExpandedBackfillCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *workflowExpandedBackfillCheck) Name() string { return "workflow-expanded-backfill" }

func (c *workflowExpandedBackfillCheck) CanFix() bool { return true }

func (c *workflowExpandedBackfillCheck) WarmupEligible() bool { return false }

// expandedBackfillTarget is one unmarked expanded root in its authoritative store.
type expandedBackfillTarget struct {
	ref     storeref.StoreRef
	store   beads.Store
	beadID  string
	members int
}

// expandedRootCandidate is a workflow root id one census leg listed.
type expandedRootCandidate struct {
	ref storeref.StoreRef
	id  string
}

// openScopes opens the city work store and every active rig store. Open
// failures are reported, never dropped.
//
// residency:allow — a constructor INPUT, not a residency answer: it opens the
// scope stores the doctor is configured with and hands them to the topology
// constructor, consulting no binding, namespace or leg order. The same shape
// as servingRigStores.
func (c *workflowExpandedBackfillCheck) openScopes() (work beads.Store, rigs map[string]beads.Store, skipped []string) {
	if c.newStore == nil || strings.TrimSpace(c.cityPath) == "" {
		return nil, nil, []string{"city skipped: no bead store factory"}
	}
	work, err := c.newStore(c.cityPath)
	if err != nil {
		return nil, nil, []string{fmt.Sprintf("city skipped: opening bead store: %v", err)}
	}
	if c.cfg == nil {
		return work, nil, nil
	}
	rigs = make(map[string]beads.Store, len(c.cfg.Rigs))
	for _, rig := range c.cfg.Rigs {
		if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
			continue
		}
		store, err := c.newStore(rig.Path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("rig %s skipped: opening bead store: %v", rig.Name, err))
			continue
		}
		rigs[rig.Name] = store
	}
	return work, rigs, skipped
}

func (c *workflowExpandedBackfillCheck) resolveTopology(work beads.Store, rigs map[string]beads.Store) storeref.Topology {
	if c.topology != nil {
		return c.topology(work, rigs)
	}
	return residencyTopologyForCity(c.cityPath, c.cfg, work, rigs)
}

func (c *workflowExpandedBackfillCheck) collect() (targets []expandedBackfillTarget, skipped []string) {
	work, rigs, skipped := c.openScopes()
	if work == nil {
		return nil, skipped
	}
	topo := c.resolveTopology(work, rigs)
	plan, err := storeref.Plan(storeref.Census{Classes: []coordclass.Class{coordclass.ClassGraph}}, topo)
	if err != nil {
		return nil, append(skipped, fmt.Sprintf("graph stores skipped: %v", err))
	}
	// No dedupe here: a twin listed by two legs must reach the by-id placement
	// below rather than be folded onto whichever leg the census reads first.
	// Non-closed only; a closed root is not ready and needs no protection.
	census, err := storeref.Union(plan, nil, func(leg storeref.Leg) ([]expandedRootCandidate, error) {
		roots, err := beads.HandlesFor(leg.Store).Live.List(beads.ListQuery{
			Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
			TierMode: beads.FederatedReadTier,
		})
		if err != nil {
			return nil, err
		}
		out := make([]expandedRootCandidate, 0, len(roots))
		for _, root := range roots {
			out = append(out, expandedRootCandidate{ref: leg.Ref, id: root.ID})
		}
		return out, nil
	})
	for _, legErr := range census.LegErrors {
		skipped = append(skipped, fmt.Sprintf("%s skipped: listing workflow roots: %v", storeRefName(legErr.Ref), legErr.Err))
	}
	if err != nil {
		return nil, append(skipped, fmt.Sprintf("workflow roots skipped: %v", err))
	}

	seen := make(map[string]bool, len(census.Items))
	for _, cand := range census.Items {
		if seen[cand.id] {
			continue
		}
		seen[cand.id] = true
		owner, root, err := c.authoritativeRoot(topo, work, cand.id)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("root %s (listed in %s) skipped: %v", cand.id, storeRefName(cand.ref), err))
			continue
		}
		if !isUnmarkedGraphV2Root(root) {
			continue
		}
		members, err := countWorkflowMembers(owner.Store, root)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("root %s skipped: listing members in %s: %v", root.ID, storeRefName(owner.Ref), err))
			continue
		}
		if members == 0 {
			continue
		}
		targets = append(targets, expandedBackfillTarget{ref: owner.Ref, store: owner.Store, beadID: root.ID, members: members})
	}
	return targets, skipped
}

// authoritativeRoot places id through the by-id resolver and returns the row
// from the store that owns it. The work residual is handed back unprobed, so
// it is read here; a miss on every leg is an error, never a verdict.
func (c *workflowExpandedBackfillCheck) authoritativeRoot(topo storeref.Topology, work beads.Store, id string) (storeref.Owner, beads.Bead, error) {
	owner, err := byIDOwnerForTopology(topo, id, work)
	if err != nil {
		return storeref.Owner{}, beads.Bead{}, err
	}
	if owner.Read {
		return owner, owner.Bead, nil
	}
	root, err := owner.Store.Get(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return storeref.Owner{}, beads.Bead{}, fmt.Errorf("residency placed it at %s, which does not hold it", storeRefName(owner.Ref))
		}
		return storeref.Owner{}, beads.Bead{}, fmt.Errorf("reading from %s: %w", storeRefName(owner.Ref), err)
	}
	return owner, root, nil
}

// isUnmarkedGraphV2Root reports whether the authoritative row is a non-closed
// graph.v2 workflow root that still lacks the expansion stamp.
func isUnmarkedGraphV2Root(root beads.Bead) bool {
	if strings.EqualFold(strings.TrimSpace(root.Status), "closed") {
		return false
	}
	if beadmeta.IsExpandedWorkflow(root.Metadata) {
		return false
	}
	return strings.TrimSpace(root.Metadata[beadmeta.KindMetadataKey]) == beadmeta.KindWorkflow &&
		strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]) == beadmeta.FormulaContractGraphV2
}

// countWorkflowMembers counts the beads in the root's own store whose
// gc.root_bead_id names it, excluding the root itself and, when the root
// carries gc.root_store_ref, any member stamped with a different one. Closed
// members are read: a finalizer-waiting root's steps are all closed.
func countWorkflowMembers(store beads.Store, root beads.Bead) (int, error) {
	members, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		IncludeClosed: true,
		TierMode:      beads.FederatedReadTier,
	})
	if err != nil {
		return 0, err
	}
	rootRef := strings.TrimSpace(root.Metadata[beadmeta.RootStoreRefMetadataKey])
	n := 0
	for _, m := range members {
		if m.ID == root.ID {
			continue
		}
		if rootRef != "" && strings.TrimSpace(m.Metadata[beadmeta.RootStoreRefMetadataKey]) != rootRef {
			continue
		}
		n++
	}
	return n, nil
}

func (c *workflowExpandedBackfillCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	targets, skipped := c.collect()
	if len(targets) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no expanded workflow roots lack gc.workflow_expanded")
	}
	details := make([]string, 0, len(targets)+len(skipped))
	for _, tgt := range targets {
		details = append(details, fmt.Sprintf("%s root %s has %d member bead(s) but no gc.workflow_expanded", storeRefName(tgt.ref), tgt.beadID, tgt.members))
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(targets) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("gc.workflow_expanded backfill skipped %d store(s)/root(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d expanded workflow root(s) lack gc.workflow_expanded and remain claimable as fresh work", len(targets)),
		"run gc doctor --check workflow-expanded-backfill --fix to stamp gc.workflow_expanded=true on expanded roots",
		details)
}

func (c *workflowExpandedBackfillCheck) Fix(_ *doctor.CheckContext) error {
	targets, skipped := c.collect()
	for _, tgt := range targets {
		if err := tgt.store.SetMetadata(tgt.beadID, beadmeta.WorkflowExpandedMetadataKey, "true"); err != nil {
			return fmt.Errorf("%s root %s: stamp gc.workflow_expanded: %w", storeRefName(tgt.ref), tgt.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("%s skipped %d store(s)/root(s): %s", c.Name(), len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}

// storeRefName renders a store ref for a detail line, where the work store's empty
// ref would read as a missing word.
func storeRefName(ref storeref.StoreRef) string {
	if ref == storeref.WorkRef {
		return "city work store"
	}
	return string(ref)
}
