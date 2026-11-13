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

type enrichServer struct {
	rulesFail, baseFail, historyFail bool
}

// Serves branch rules requiring backend-tests and lint, a base tip where
// backend-tests is red, and a head history where lint also has a green run.
func (e enrichServer) serve(t *testing.T) func(http.ResponseWriter, *http.Request) {
	runs := func(w http.ResponseWriter, sha string, rs ...map[string]any) {
		for _, r := range rs {
			r["head_sha"] = sha
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(rs), "check_runs": rs})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/rules/branches/main":
			if e.rulesFail {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"type": "pull_request"},
				{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": []map[string]any{{"context": "backend-tests"}, {"context": "lint"}}}},
				{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": []map[string]any{{"context": "typecheck"}}}},
			})
		case r.URL.Path == "/repos/o/r/commits/base1/check-runs":
			if e.baseFail {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			runs(w, "base1", map[string]any{"id": 1, "name": "backend-tests", "status": "completed", "conclusion": "failure"},
				map[string]any{"id": 2, "name": "lint", "status": "completed", "conclusion": "success"})
		case r.URL.Path == "/repos/o/r/commits/base1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "statuses": []map[string]any{}})
		case r.URL.Path == "/repos/o/r/commits/head1/check-runs" && r.URL.Query().Get("filter") == "all":
			if e.historyFail {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			runs(w, "head1", map[string]any{"id": 10, "name": "lint", "status": "completed", "conclusion": "success"},
				map[string]any{"id": 11, "name": "lint", "status": "completed", "conclusion": "failure"},
				map[string]any{"id": 12, "name": "typecheck", "status": "completed", "conclusion": "failure"})
		default:
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func enriched(t *testing.T, e enrichServer, names ...string) map[string]string {
	srv := snapshotServer(t, e.serve(t))
	defer srv.Close()
	snap := &PRSnapshot{HeadSHA: "head1", BaseRef: "main", BaseSHA: "base1"}
	for _, n := range names {
		snap.Contexts = append(snap.Contexts, failing(n))
	}
	snap.Contexts = append(snap.Contexts, CheckContext{Name: "security-gate", Status: "completed", Conclusion: "success"})
	EnrichChecks(context.Background(), newTestClient(t, srv.URL), 1, "o", "r", snap)
	got := map[string]string{}
	for _, c := range snap.Contexts {
		got[c.Name] = c.RedCause
	}
	return got
}

// CHE-3257: a failing check is classified from the branch rules, the base tip
// and the other runs of the same head.
func TestEnrichChecksClassifiesFailingChecks(t *testing.T) {
	got := enriched(t, enrichServer{}, "backend-tests", "lint", "typecheck", "docs-preview")
	want := map[string]string{
		"backend-tests": CauseInfra,    // also red on the base tip
		"lint":          CauseInfra,    // green on another run of the same head
		"typecheck":     CausePR,       // required, green nowhere else
		"docs-preview":  CauseOptional, // not required
		"security-gate": "",            // not failing
	}
	for name, cause := range want {
		if got[name] != cause {
			t.Errorf("%s: RedCause = %q, want %q", name, got[name], cause)
		}
	}
}

// An unreadable read must leave the cause unknown, never PR-caused.
func TestEnrichChecksLeavesCauseUnknownWhenEvidenceIsUnreadable(t *testing.T) {
	cases := []struct {
		name  string
		e     enrichServer
		check string
		want  string
	}{
		{"rules unreadable", enrichServer{rulesFail: true}, "typecheck", ""},
		{"base tip unreadable", enrichServer{baseFail: true}, "typecheck", ""},
		{"run history unreadable", enrichServer{historyFail: true}, "typecheck", ""},
		{"base unreadable but history shows a green run", enrichServer{baseFail: true}, "lint", CauseInfra},
		{"history unreadable but base shows it red", enrichServer{historyFail: true}, "backend-tests", CauseInfra},
	}
	for _, c := range cases {
		if got := enriched(t, c.e, c.check)[c.check]; got != c.want {
			t.Errorf("%s: RedCause = %q, want %q", c.name, got, c.want)
		}
	}
}
