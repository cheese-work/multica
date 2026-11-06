package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/ciwatch"
	"github.com/multica-ai/multica/server/internal/featureflags"
)

type ciFakeGH struct{}

func (ciFakeGH) ListJobs(context.Context, ciwatch.Run, int) ([]ciwatch.Job, bool, http.Header, error) {
	return []ciwatch.Job{{ID: 7, Name: "build", Conclusion: "failure"}}, false, nil, nil
}

func (ciFakeGH) JobLog(context.Context, ciwatch.Run, int64) (io.ReadCloser, http.Header, error) {
	return io.NopCloser(strings.NewReader("API rate limit exceeded\n")), nil, nil
}

type ciFakeBudget struct{}

func (ciFakeBudget) Reserve(context.Context, int64, int) (ciwatch.Release, error) {
	return func(http.Header) error { return nil }, nil
}
func (ciFakeBudget) Limited(context.Context, int64, *ciwatch.RateLimitError) error { return nil }

// ciWatchFixture binds the installation, turns the flag on, watches the repo in
// shadow mode and installs a watcher backed by a fake GitHub. It returns the
// test workspace id and a post func for a completed run.
func ciWatchFixture(t *testing.T, installationID int64, slug string, flagOn, configured bool) (ws pgtype.UUID, post func(conclusion string) int) {
	t.Helper()
	ws, _ = workflowFanoutFixture(t, installationID, slug)
	withFeatureFlag(t, testHandler, featureflags.CIWatcher, flagOn)
	ctx := context.Background()
	if configured {
		if _, err := testPool.Exec(ctx, `INSERT INTO ci_watch_config (workspace_id, repo_owner, repo_name, mode) VALUES ($1, 'cheese-work', 'multica', 'shadow')`, ws); err != nil {
			t.Fatal(err)
		}
	}
	testHandler.ciWatch = testHandler.newCIWatcher(ciFakeGH{}, ciFakeBudget{})
	t.Cleanup(func() {
		testHandler.ciWatch = nil
		testPool.Exec(ctx, `DELETE FROM ci_watch_config WHERE workspace_id = $1`, ws)
		testPool.Exec(ctx, `DELETE FROM system1_observation WHERE workspace_id = $1 AND kind = 'ci_event'`, ws)
		testPool.Exec(ctx, `DELETE FROM ci_failure_event WHERE run_id = $1`, cdDeployRunID)
	})
	return ws, func(conclusion string) int {
		body := workflowRunEnvelope(t, installationID, "completed", "completed")
		run := body["workflow_run"].(map[string]any)
		run["conclusion"] = conclusion
		run["path"], run["name"], run["event"], run["head_branch"] = ".github/workflows/ci.yml", "CI", "push", "main"
		body["repository"].(map[string]any)["default_branch"] = "main"
		return postWorkflowEvent(t, "workflow_run", body)
	}
}

func ciCounts(t *testing.T, ws pgtype.UUID) (events, observations int) {
	t.Helper()
	ctx := context.Background()
	testPool.QueryRow(ctx, `SELECT count(*) FROM ci_failure_event WHERE workspace_id = $1 AND run_id = $2`, ws, cdDeployRunID).Scan(&events)
	testPool.QueryRow(ctx, `SELECT count(*) FROM system1_observation WHERE workspace_id = $1 AND kind = 'ci_event' AND decision_source = 'rule' AND class = 'infra_rate_limit' AND choice_key = 'P1'`, ws).Scan(&observations)
	return
}

func TestWorkflowWebhookRecordsAFailedRunOnce(t *testing.T) {
	ws, post := ciWatchFixture(t, 9_104_276_001, "ciw-failed", true, true)
	for i := 0; i < 2; i++ { // the second delivery is a redelivery
		if code := post("failure"); code != 202 {
			t.Fatalf("delivery %d: status %d", i, code)
		}
	}
	if e, o := ciCounts(t, ws); e != 1 || o != 1 {
		t.Fatalf("events=%d observations=%d, want 1 and 1", e, o)
	}
}

func TestWorkflowWebhookRecordsACancelledRunAsP0(t *testing.T) {
	ws, post := ciWatchFixture(t, 9_104_276_002, "ciw-cancelled", true, true)
	if code := post("cancelled"); code != 202 {
		t.Fatalf("status %d", code)
	}
	var n int
	testPool.QueryRow(context.Background(), `SELECT count(*) FROM system1_observation WHERE workspace_id = $1 AND kind = 'ci_event' AND decision_source = 'rule'`, ws).Scan(&n)
	if n != 1 {
		t.Fatalf("observations=%d, want 1", n)
	}
	if e, _ := ciCounts(t, ws); e != 0 {
		t.Fatalf("a cancelled run stored %d evidence rows, want 0", e)
	}
}

func TestWorkflowWebhookCIWatchStaysOffWithoutFlagOrConfig(t *testing.T) {
	for name, tc := range map[string]struct{ flag, config bool }{"flag off": {false, true}, "no config row": {true, false}} {
		t.Run(name, func(t *testing.T) {
			ws, post := ciWatchFixture(t, 9_104_276_003, "ciw-off-"+strings.ReplaceAll(name, " ", "-"), tc.flag, tc.config)
			if code := post("failure"); code != 202 {
				t.Fatalf("status %d", code)
			}
			var n int
			testPool.QueryRow(context.Background(), `SELECT count(*) FROM system1_observation WHERE workspace_id = $1 AND kind = 'ci_event'`, ws).Scan(&n)
			if e, _ := ciCounts(t, ws); e != 0 || n != 0 {
				t.Fatalf("events=%d observations=%d, want none", e, n)
			}
		})
	}
}

type failOnceWatcher struct {
	next  ciWatcher
	fails int
}

func (w *failOnceWatcher) Watch(ctx context.Context, ws pgtype.UUID, run ciwatch.Run, def string) error {
	if w.fails > 0 {
		w.fails--
		return errors.New("synthetic watcher failure")
	}
	return w.next.Watch(ctx, ws, run, def)
}

func TestWorkflowWebhookWatcherFailureReturns5xxAndRedeliversCleanly(t *testing.T) {
	ws, post := ciWatchFixture(t, 9_104_276_004, "ciw-retry", true, true)
	testHandler.ciWatch = &failOnceWatcher{next: testHandler.ciWatch, fails: 1}
	if code := post("failure"); code < 500 {
		t.Fatalf("failed watch answered %d, want 5xx", code)
	}
	if code := post("failure"); code != 202 {
		t.Fatalf("redelivery answered %d", code)
	}
	if e, o := ciCounts(t, ws); e != 1 || o != 1 {
		t.Fatalf("events=%d observations=%d, want 1 and 1", e, o)
	}
}
