package herdr

import (
	"encoding/json"
	"testing"
)

// "name" is the registered session identity; "agent" is the detected
// provider kind. An unnamed pane must not inherit the shared kind as its name.
func TestAgentInfoKeepsRegisteredNameSeparateFromKind(t *testing.T) {
	var got []agentInfo
	body := `[{"agent":"opencode","name":"mayor","pane_id":"w1:p1"},{"agent":"opencode","name":"worker-1","pane_id":"w1:p2"},{"agent":"opencode","pane_id":"w1:p3"}]`
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"mayor", "worker-1", ""}
	for i, want := range wantNames {
		if got[i].Name != want || got[i].Kind != "opencode" || got[i].PaneID == "" {
			t.Fatalf("entry %d = %+v; want name %q, kind opencode", i, got[i], want)
		}
	}
}
