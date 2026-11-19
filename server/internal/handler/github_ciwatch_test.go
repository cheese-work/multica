package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/ciwatch"
	"github.com/multica-ai/multica/server/internal/featureflags"
)

// ciFakeGH answers every run with one failed job whose log is log, and counts
// the GitHub calls it received.
type ciFakeGH struct {
	log   string
	calls atomic.Int32
}

func (g *ciFakeGH) ListJobs(context.Context, ciwatch.Run, int) ([]ciwatch.Job, bool, http.Header, error) {
	g.calls.Add(1)
	return []ciwatch.Job{{ID: 7, Name: "build", Conclusion: "failure"}}, false, nil, nil
}

func (g *ciFakeGH) JobLog(context.Context, ciwatch.Run, int64) (io.ReadCloser, http.Header, error) {
	g.calls.Add(1)
	return io.NopCloser(strings.NewReader(g.log)), nil, nil
}

type ciFakeBudget struct{}

func (ciFakeBudget) Reserve(context.Context, int64, int) (ciwatch.Release, error) {
	return func(http.Header) error { return nil }, nil
}
func (ciFakeBudget) Limited(context.Context, int64, *ciwatch.RateLimitError) error { return nil }

// ciWatchFixture binds the installation, turns the flag on, watches the repo in
// shadow mode and installs a watcher backed by a fake GitHub. It returns the
// test workspace id, the fake GitHub and a post func for a completed run.
func ciWatchFixture(t *testing.T, installationID int64, slug string, flagOn, configured bool) (ws pgtype.UUID, post func(conclusion string) int) {
	ws, _, postRun := ciWatchFixtureLog(t, installationID, slug, flagOn, configured, "API rate limit exceeded\n")
	return ws, func(conclusion string) int { return postRun(cdDeployRunID, conclusion) }
}

func ciWatchFixtureLog(t *testing.T, installationID int64, slug string, flagOn, configured bool, log string) (ws pgtype.UUID, gh *ciFakeGH, post func(runID int64, conclusion string) int) {
	t.Helper()
	ws, _ = workflowFanoutFixture(t, installationID, slug)
	withFeatureFlag(t, testHandler, featureflags.CIWatcher, flagOn)
	ctx := context.Background()
	// Earlier tests in the shard may leave ci_event rows in the shared workspace.
	testPool.Exec(ctx, `DELETE FROM system1_observation WHERE workspace_id = $1 AND kind = 'ci_event'`, ws)
	if configured {
		if _, err := testPool.Exec(ctx, `INSERT INTO ci_watch_config (workspace_id, repo_owner, repo_name, mode) VALUES ($1, 'cheese-work', 'multica', 'shadow')`, ws); err != nil {
			t.Fatal(err)
		}
	}
	gh = &ciFakeGH{log: log}
	testHandler.ciWatch = testHandler.newCIWatcher(gh, ciFakeBudget{})
	t.Cleanup(func() {
		testHandler.ciWatch = nil
		testPool.Exec(ctx, `DELETE FROM ci_watch_config WHERE workspace_id = $1`, ws)
		testPool.Exec(ctx, `DELETE FROM system1_observation WHERE workspace_id = $1 AND kind = 'ci_event'`, ws)
		testPool.Exec(ctx, `DELETE FROM ci_failure_event WHERE workspace_id = $1`, ws)
		testPool.Exec(ctx, `DELETE FROM ci_flake_increment WHERE workspace_id = $1`, ws)
		testPool.Exec(ctx, `DELETE FROM github_workflow_run WHERE repo_owner = 'cheese-work' AND repo_name = 'multica' AND run_id BETWEEN $1 AND $1 + 10`, cdDeployRunID)
	})
	return ws, gh, func(runID int64, conclusion string) int {
		body := workflowRunEnvelope(t, installationID, "completed", "completed")
		run := body["workflow_run"].(map[string]any)
		run["id"], run["conclusion"] = runID, conclusion
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

// The shared test workspace may already hold ci_event rows from tests that ran
// earlier in the shard; a cancelled run must be judged on its own row only.
func TestWorkflowWebhookCancelledRunCountIgnoresRowsFromEarlierTests(t *testing.T) {
	seedCIEvent(t, "stray-a")
	seedCIEvent(t, "stray-b")
	TestWorkflowWebhookRecordsACancelledRunAsP0(t)
}

func TestWorkflowWebhookCIWatchStaysOffWithoutFlagOrConfig(t *testing.T) {
	for name, tc := range map[string]struct{ flag, config bool }{"flag off": {false, true}, "no config row": {true, false}} {
		t.Run(name, func(t *testing.T) {
			ws, gh, post := ciWatchFixtureLog(t, 9_104_276_003, "ciw-off-"+strings.ReplaceAll(name, " ", "-"), tc.flag, tc.config, "API rate limit exceeded\n")
			if code := post(cdDeployRunID, "failure"); code != 202 {
				t.Fatalf("status %d", code)
			}
			if n := gh.calls.Load(); n != 0 {
				t.Fatalf("%d GitHub calls, want none", n)
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

// obsFor returns the class and label of the observation of one run.
func obsFor(t *testing.T, ws pgtype.UUID, runID int64) (class, label string) {
	t.Helper()
	err := testPool.QueryRow(context.Background(), `SELECT class, coalesce(outcome_label,'') FROM system1_observation
		WHERE workspace_id = $1 AND kind = 'ci_event' AND (inputs->>'run_id')::bigint = $2`, ws, runID).Scan(&class, &label)
	if err != nil {
		t.Fatalf("observation of run %d: %v", runID, err)
	}
	return
}

func TestWorkflowWebhookThreeFlakesClassifyTheNextFailureAsFlaky(t *testing.T) {
	ws, _, post := ciWatchFixtureLog(t, 9_104_276_005, "ciw-flaky", true, true, "--- FAIL: TestX (0.01s)\nFAIL\n")
	first, second := cdDeployRunID+1, cdDeployRunID+2
	if code := post(first, "failure"); code != 202 {
		t.Fatalf("status %d", code)
	}
	if class, _ := obsFor(t, ws, first); class != "code_or_test" {
		t.Fatalf("first failure class = %q, want code_or_test", class)
	}
	var sig string
	if err := testPool.QueryRow(context.Background(), `SELECT signature_hash FROM ci_failure_event WHERE workspace_id = $1 AND run_id = $2`, ws, first).Scan(&sig); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := testPool.Exec(context.Background(), `INSERT INTO ci_flake_increment (workspace_id, repo_owner, repo_name, signature_hash, reason, ref)
			VALUES ($1, 'cheese-work', 'multica', $2, 'green_rerun', $3)`, ws, sig, "run-"+strings.Repeat("x", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if code := post(second, "failure"); code != 202 {
		t.Fatalf("status %d", code)
	}
	if class, _ := obsFor(t, ws, second); class != "flaky" {
		t.Fatalf("next failure class = %q, want flaky", class)
	}
}

func TestWorkflowWebhookLaterGreenRunLabelsTheFailureSelfHealed(t *testing.T) {
	ws, _, post := ciWatchFixtureLog(t, 9_104_276_006, "ciw-healed", true, true, "--- FAIL: TestX (0.01s)\nFAIL\n")
	failed, green := cdDeployRunID+1, cdDeployRunID+2
	if code := post(failed, "failure"); code != 202 {
		t.Fatalf("status %d", code)
	}
	if _, label := obsFor(t, ws, failed); label != "" {
		t.Fatalf("label before the green run = %q", label)
	}
	if code := post(green, "success"); code != 202 {
		t.Fatalf("status %d", code)
	}
	if _, label := obsFor(t, ws, failed); label != "self_healed" {
		t.Fatalf("label = %q, want self_healed", label)
	}
}

func TestWorkflowWebhookPlantedSecretReachesNoRowAndNoLog(t *testing.T) {
	const (
		token  = "ghp_PLANTEDsecretTOKEN0123456789abcdefABCD" // gitleaks:allow
		bearer = "plantedBEARERvalue12345"
	)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ws, _, post := ciWatchFixtureLog(t, 9_104_276_007, "ciw-secret", true, true,
		"--- FAIL: TestX\nGH_TOKEN: "+token+"\nAuthorization: Bearer "+bearer+"\nFAIL\n")
	if code := post(cdDeployRunID, "failure"); code != 202 {
		t.Fatalf("status %d", code)
	}
	var rows string
	if err := testPool.QueryRow(context.Background(), `SELECT
		coalesce((SELECT string_agg(e::text, ' ') FROM ci_failure_event e WHERE e.workspace_id = $1), '') ||
		coalesce((SELECT string_agg(o::text, ' ') FROM system1_observation o WHERE o.workspace_id = $1 AND o.kind = 'ci_event'), '')`, ws).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rows, "TestX") {
		t.Fatalf("no evidence was stored, the check proves nothing: %s", rows)
	}
	for _, secret := range []string{token, bearer} {
		if strings.Contains(rows, secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("planted secret %q reached a row or the server log", secret)
		}
	}
}
