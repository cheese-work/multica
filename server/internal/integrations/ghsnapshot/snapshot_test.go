package ghsnapshot

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestSnapshotDecided(t *testing.T) {
	cases := []struct {
		name string
		snap PRSnapshot
		want bool
	}{
		{"clean passed", PRSnapshot{Mergeable: "MERGEABLE", HasChecks: true, RollupState: "SUCCESS",
			Contexts: []CheckContext{{Status: "completed", Conclusion: "success"}}}, true},
		{"conflicting decided", PRSnapshot{Mergeable: "CONFLICTING", HasChecks: false}, true},
		{"mergeable unknown", PRSnapshot{Mergeable: "UNKNOWN", HasChecks: true, RollupState: "SUCCESS"}, false},
		{"rollup pending", PRSnapshot{Mergeable: "MERGEABLE", HasChecks: true, RollupState: "PENDING"}, false},
		{"running context", PRSnapshot{Mergeable: "MERGEABLE", HasChecks: true, RollupState: "SUCCESS",
			Contexts: []CheckContext{{Status: "in_progress"}}}, false},
		{"no checks but mergeable", PRSnapshot{Mergeable: "MERGEABLE", HasChecks: false}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snap.Decided(); got != tc.want {
				t.Fatalf("Decided() = %v, want %v", got, tc.want)
			}
		})
	}
}

// CHE-1874: the GitHub id names one finished result. A check run and a commit
// status both carry it, so a rerun with the same name, state, URL and time still
// has a new identity. A status that arrives without an id carries none.
func TestFetchPRSnapshotCarriesTheGitHubIDOfEachCheck(t *testing.T) {
	srv := snapshotServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/5":
			writeTestPR(w, "sha1", true, "clean")
		case "/repos/o/r/commits/sha1/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]any{
				{"id": 4711, "head_sha": "sha1", "name": "build", "status": "completed", "conclusion": "success", "details_url": "https://x/job/1"},
			}})
		case "/repos/o/r/commits/sha1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "statuses": []map[string]any{
				{"id": 99, "context": "ci", "state": "success", "target_url": "https://x/7"},
				{"context": "legacy", "state": "success"},
			}})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()
	snap, err := FetchPRSnapshot(context.Background(), newTestClient(t, srv.URL), 1, "o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range snap.Contexts {
		got[c.Name] = c.ExternalID
	}
	if got["build"] != "4711" || got["ci"] != "99" || got["legacy"] != "" {
		t.Fatalf("external ids = %v, want build=4711 ci=99 legacy=\"\"", got)
	}
}
