package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reader limit must not let structural roots hide executable work. Model a
// reader that honors --limit, unlike the fixed-response query fixtures.
func TestWorkflowRootAdmissionFiltersBeforeLimit(t *testing.T) {
	a := Agent{Name: "worker"}
	for _, mode := range []string{"child-owned", "ready-sibling", "root-only"} {
		t.Run(mode, func(t *testing.T) {
			script := `#!/bin/sh
set -eu
case "$*" in
  *"gc.routed_to=worker"*) ;;
  *) printf '[]'; exit 0 ;;
esac
limit=0
for arg in "$@"; do
  case "$arg" in --limit=*) limit=${arg#--limit=} ;; esac
done
jq -nc --arg mode "$MODE" --argjson limit "$limit" '
  [range(0; 25) | {id:("root-" + tostring),status:"open",metadata:{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.workflow_expanded":"true","gc.routed_to":"worker"}}]
  + (if $mode == "ready-sibling" then [{id:"step",status:"open",metadata:{"gc.routed_to":"worker"}}]
     elif $mode == "root-only" then [{id:"launch",status:"open",metadata:{"gc.kind":"workflow","gc.routed_to":"worker"}}]
     else [] end)
  | if $limit > 0 then .[:$limit] else . end'
`
			env := map[string]string{"MODE": mode}
			out := runEffectiveWorkQuery(t, a, env, script)
			ids := workQueryOutputIDOrder(t, out)
			want := ""
			switch mode {
			case "ready-sibling":
				want = "step"
			case "root-only":
				want = "launch"
			}
			if strings.Join(ids, ",") != want {
				t.Fatalf("work IDs = %v, want %q", ids, want)
			}
			count := strings.TrimSpace(runShellWithFakeBd(t, a.EffectivePoolDemandQuery(), env, script))
			wantCount := "1"
			if mode == "child-owned" {
				wantCount = "0"
			}
			if count != wantCount {
				t.Fatalf("demand = %q, want %s", count, wantCount)
			}
		})
	}
}

func TestWorkflowRootAdmissionRefillsOnlyAnExcludedFullWindow(t *testing.T) {
	a := Agent{Name: "worker"}
	for _, tc := range []struct {
		name      string
		tier      string
		roots     string
		wantReads string
	}{
		{"ordinary", "canonical", "0", "20"},
		{"mixed-window", "canonical", "2", "20"},
		{"excluded-full-window", "canonical", "25", "20,0"},
		{"legacy-excluded-full-window", "legacy", "25", "20,0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "reads")
			script := `#!/bin/sh
set -eu
case "$TIER:$*" in
  canonical:*"gc.routed_to=worker"*|legacy:*"gc.run_target=worker"*) ;;
  *) printf '[]'; exit 0 ;;
esac
limit=0
for arg in "$@"; do
  case "$arg" in --limit=*) limit=${arg#--limit=} ;; esac
done
printf '%s\n' "$limit" >> "$READ_LOG"
jq -nc --arg tier "$TIER" --argjson limit "$limit" --argjson roots "$ROOTS" '
  [range(0; $roots) | {id:("root-" + tostring),metadata:{"gc.kind":"workflow","gc.workflow_expanded":"true"}}]
  + [{id:"work",metadata:{"gc.kind":(if $tier == "legacy" then "workflow" else "task" end)}}]
  | if $limit > 0 then .[:$limit] else . end'
`
			out := runEffectiveWorkQuery(t, a, map[string]string{"READ_LOG": log, "ROOTS": tc.roots, "TIER": tc.tier}, script)
			if ids := workQueryOutputIDOrder(t, out); strings.Join(ids, ",") != "work" {
				t.Fatalf("work IDs = %v, want work", ids)
			}
			reads, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(strings.Fields(string(reads)), ","); got != tc.wantReads {
				t.Fatalf("read limits = %s, want %s", got, tc.wantReads)
			}
		})
	}
}

func TestWorkflowRootAdmissionMetadataRepresentation(t *testing.T) {
	query := `printf '%s' "$ROWS" | jq -c '` + poolDemandAdmissionJQ() + `'`
	rows := `[{"id":"boolean","metadata":{"gc.kind":"workflow","gc.workflow_expanded":true}},
{"id":"whitespace","metadata":{"gc.kind":" workflow ","gc.workflow_expanded":" true "}},
{"id":"task","metadata":{"gc.kind":"task","gc.workflow_expanded":"true"}},
{"id":"root-only","metadata":{"gc.kind":"workflow"}}]`
	out := runShellWithFakeBd(t, query, map[string]string{"ROWS": rows}, "#!/bin/sh\nexit 99\n")
	if got := strings.Join(workQueryOutputIDOrder(t, out), ","); got != "task,root-only" {
		t.Fatalf("admitted IDs = %s, want task,root-only", got)
	}
}
