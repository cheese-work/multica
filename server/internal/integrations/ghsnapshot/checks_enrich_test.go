package ghsnapshot

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func failing(name string) CheckContext {
	return CheckContext{Name: name, Status: "completed", Conclusion: "failure"}
}

// CHE-3257: the snapshot learns which checks are required (branch rules) and
// which of them are also red on the base tip.
func TestEnrichChecks(t *testing.T) {
	baseCalls := 0
	srv := snapshotServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/rules/branches/main":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"type": "pull_request"},
				{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": []map[string]any{{"context": "backend-tests"}, {"context": "lint"}}}},
				{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": []map[string]any{{"context": "security-gate"}}}},
			})
		case "/repos/o/r/commits/base1/check-runs":
			baseCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []map[string]any{
				{"id": 1, "head_sha": "base1", "name": "backend-tests", "status": "completed", "conclusion": "failure"},
				{"id": 2, "head_sha": "base1", "name": "lint", "status": "completed", "conclusion": "success"},
			}})
		case "/repos/o/r/commits/base1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "statuses": []map[string]any{}})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()
	snap := &PRSnapshot{BaseRef: "main", BaseSHA: "base1", Contexts: []CheckContext{
		failing("backend-tests"), failing("lint"), failing("docs-preview"), {Name: "security-gate", Status: "completed", Conclusion: "success"},
	}}
	EnrichChecks(context.Background(), newTestClient(t, srv.URL), 1, "o", "r", snap)
	want := map[string][2]bool{ // name -> {Required, RedOnBase}
		"backend-tests": {true, true}, "lint": {true, false}, "docs-preview": {false, false}, "security-gate": {true, false},
	}
	for _, c := range snap.Contexts {
		if got := [2]bool{c.Required, c.RedOnBase}; got != want[c.Name] {
			t.Errorf("%s: {Required, RedOnBase} = %v, want %v", c.Name, got, want[c.Name])
		}
	}
}

// A rules read that fails must not hide a red: every check counts as required.
func TestEnrichChecksFailsClosedWhenRulesUnreadable(t *testing.T) {
	srv := snapshotServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	defer srv.Close()
	snap := &PRSnapshot{BaseRef: "main", BaseSHA: "base1", Contexts: []CheckContext{failing("docs-preview")}}
	EnrichChecks(context.Background(), newTestClient(t, srv.URL), 1, "o", "r", snap)
	if c := snap.Contexts[0]; !c.Required || c.RedOnBase {
		t.Errorf("got Required=%v RedOnBase=%v, want true,false", c.Required, c.RedOnBase)
	}
}
