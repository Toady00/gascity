package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestWorkflowRootAdmissionAtClaimBoundary(t *testing.T) {
	root := beads.Bead{ID: "root", Status: "open", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.workflow_expanded": "true", "gc.routed_to": "worker",
	}}
	child := beads.Bead{ID: "step", Status: "open", Metadata: map[string]string{
		"gc.root_bead_id": "root", "gc.routed_to": "worker",
	}}
	opts := hookClaimOptions{Assignee: "second", IdentityCandidates: []string{"second"}, RouteTargets: []string{"worker"}}
	var attempts []string
	ops := hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
			attempts = append(attempts, id)
			return beads.Bead{ID: id, Status: "in_progress", Assignee: "first"}, false, nil
		},
		EmitClaimRejected: func(string, string, string) {},
	}
	var stdout, stderr bytes.Buffer
	result := claimFirstEligibleHookCandidate([]beads.Bead{child, root}, opts, ops, "unused", &stdout, &stderr)
	if result.terminal || len(attempts) != 1 || attempts[0] != "step" {
		t.Fatalf("lost child claim fell through to root: result=%+v attempts=%v", result, attempts)
	}
	if demandRowServable(root) || hookCandidateVisible(root, opts.IdentityCandidates, opts.RouteTargets) {
		t.Fatal("expanded root is still fresh demand or visible work")
	}
	root.Assignee = "second"
	root.Status = "in_progress"
	if !hookCandidateVisible(root, opts.IdentityCandidates, opts.RouteTargets) {
		t.Fatal("existing owner lost its anchor")
	}
	if _, _, ok := hookClaimExistingAssignment([]beads.Bead{root}, opts); !ok {
		t.Fatal("existing assignment must remain adoptable")
	}
	root.Assignee = "first"
	if hookCandidateReclaimEligible(root, opts.RouteTargets, time.Now()) {
		t.Fatal("stale-owner reclaim bypasses root admission")
	}
	root.Assignee = ""
	delete(root.Metadata, "gc.workflow_expanded")
	if !hookCandidateClaimable(root, opts.RouteTargets, time.Now()) || !demandRowServable(root) {
		t.Fatal("root-only launch must remain claimable and count as demand")
	}
}
