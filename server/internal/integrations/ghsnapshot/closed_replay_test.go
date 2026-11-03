package ghsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-3402: a pull_request "closed" delivery that never landed left the row
// open while the refresh kept reading a merged PR from GitHub.
func TestFetchPRSnapshotReportsClosed(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  bool
	}{{"open", false}, {"closed", true}} {
		t.Run(tc.state, func(t *testing.T) {
			srv := snapshotServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/o/r/pulls/5":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"state": tc.state, "head": map[string]any{"sha": "sha1"},
						"base": map[string]any{"ref": "main", "sha": "base-tip"}, "mergeable": nil, "mergeable_state": "unknown",
					})
				case "/repos/o/r/commits/sha1/check-runs":
					_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
				case "/repos/o/r/commits/sha1/status":
					_, _ = w.Write([]byte(`{"total_count":0,"statuses":[]}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			defer srv.Close()
			snap, err := FetchPRSnapshot(context.Background(), newTestClient(t, srv.URL), 1, "o", "r", 5)
			if err != nil {
				t.Fatal(err)
			}
			if snap.Closed != tc.want {
				t.Fatalf("Closed = %v, want %v", snap.Closed, tc.want)
			}
		})
	}
}

func TestProcessReportsClosedPRStillStoredOpen(t *testing.T) {
	for _, tc := range []struct {
		name     string
		closed   bool
		rowState string
		want     int
	}{
		{"closed on GitHub, open row", true, "open", 1},
		{"closed on GitHub, draft row", true, "draft", 1},
		{"closed on GitHub, merged row", true, "merged", 0},
		{"closed on GitHub, closed row", true, "closed", 0},
		{"open on GitHub, open row", false, "open", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(enabledClient(t), nil, failingBegin{}, nil)
			m.jitter = func() time.Duration { return 0 }
			// Each row's snapshot write fails without a DB; the stale-state
			// report must not depend on that write.
			m.listRows = func(context.Context, db.ListGitHubPRRowsByAddressParams) ([]db.ListGitHubPRRowsByAddressRow, error) {
				return []db.ListGitHubPRRowsByAddressRow{{State: tc.rowState}}, nil
			}
			m.fetch = func(context.Context, *Client, int64, string, string, int32) (*PRSnapshot, error) {
				return &PRSnapshot{HeadSHA: "sha1", Closed: tc.closed, Mergeable: "UNKNOWN"}, nil
			}
			var got []address
			m.SetOnClosed(func(_ context.Context, installationID int64, owner, repo string, number int32) {
				got = append(got, address{InstallationID: installationID, Owner: owner, Repo: repo, Number: number})
			})
			addr := address{InstallationID: 1, Owner: "o", Repo: "r", Number: 5}
			m.process(context.Background(), addr)
			if len(got) != tc.want {
				t.Fatalf("onClosed calls = %d, want %d", len(got), tc.want)
			}
			if tc.want == 1 && got[0] != addr {
				t.Fatalf("onClosed address = %+v, want %+v", got[0], addr)
			}
		})
	}
}

type failingBegin struct{}

func (failingBegin) Begin(context.Context) (pgx.Tx, error) { return nil, errors.New("no database") }
