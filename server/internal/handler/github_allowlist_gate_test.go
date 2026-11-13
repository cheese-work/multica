package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-3419: every GitHub consumer past intake skips an installation whose
// binding fails the allowlist policy. Each case runs unlisted -> skipped,
// listed -> unchanged, policy off -> unchanged.

type gateCase struct {
	name    string
	list    string
	account int64
	allowed bool
}

var gateCases = []gateCase{
	{"unlisted account", "7001", 7999, false},
	{"listed account", "7001", 7001, true},
	{"policy off", "", 7999, true},
}

// gatePolicy returns a copy of base with the policy set to list.
func gatePolicy(t *testing.T, base *Handler, list string) *Handler {
	t.Helper()
	h := *base
	if err := h.ConfigureGitHubAllowlist(list); err != nil {
		t.Fatalf("configure allowlist %q: %v", list, err)
	}
	// The copy shares base's refresh manager; drop the filter that points at the copy.
	t.Cleanup(func() { h.PRRefresh.SetInstallationFilter(nil) })
	return &h
}

func bindAccount(t *testing.T, installationID, accountID int64) {
	t.Helper()
	dbfx.Exec(t, `UPDATE github_installation SET account_id=$1 WHERE installation_id=$2`, accountID, installationID)
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestGitHubGateIntakeWorkerRoutesNoAgentForDisallowedInstallation(t *testing.T) {
	for _, tc := range gateCases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGitHubIntakeFixture(t)
			bindAccount(t, 2019001, tc.account)
			delivery := postIntakeForWorker(t, fixture, "-gate-"+tc.name)
			worker := intakeWorkerForTest(t, fixture)
			worker.handler = gatePolicy(t, fixture.handler, tc.list)
			if _, err := worker.runOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tc.allowed {
				assertIntakeWorkerState(t, delivery, "dispatched")
				assertIntakeWorkerDispatch(t, fixture, 1)
				return
			}
			assertIntakeWorkerState(t, delivery, "denied")
			assertIntakeWorkerDispatch(t, fixture, 0)
		})
	}
}

func TestGitHubGateReviewProjection(t *testing.T) {
	for _, tc := range gateCases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGitHubReviewFixture(t)
			bindAccount(t, 2019001, tc.account)
			body, err := json.Marshal(fixture.payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := gatePolicy(t, fixture.handler, tc.list).handlePullRequestReviewEvent(context.Background(), body); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.allowed {
				want = 1
			}
			if got := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, fixture.issueID, fixture.author); got != want {
				t.Fatalf("author runs = %d, want %d", got, want)
			}
		})
	}
}

// Two workspaces share the installation; only the listed binding gets a write.
func TestGitHubGateWorkflowEventsWriteOnlyAllowedBindings(t *testing.T) {
	const installationID = int64(341900001)
	first, second := workflowFanoutFixture(t, installationID, "wf-gate-b")
	dbfx.Exec(t, `UPDATE github_installation SET account_id=7001 WHERE workspace_id=$1 AND installation_id=$2`, first, installationID)
	dbfx.Exec(t, `UPDATE github_installation SET account_id=7999 WHERE workspace_id=$1 AND installation_id=$2`, second, installationID)
	h := gatePolicy(t, testHandler, "7001")
	run, _ := json.Marshal(workflowRunEnvelope(t, installationID, "completed", "completed"))
	job, _ := json.Marshal(workflowJobEnvelope(t, installationID, "completed"))
	if err := h.handleWorkflowRunEvent(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := h.handleWorkflowJobEvent(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, ok := workflowRunRow(t, first); !ok || workflowJobPhase(t, first) == 0 {
		t.Fatal("allowed workspace got no workflow rows")
	}
	if _, ok := workflowRunRow(t, second); ok || workflowJobPhase(t, second) != 0 {
		t.Fatal("disallowed workspace got workflow rows")
	}
}

func TestGitHubGateConfigSyncDoorbell(t *testing.T) {
	for _, tc := range gateCases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newConfigSyncWebhookFixture(t, "owner")
			bindAccount(t, fx.instID, tc.account)
			body, _ := json.Marshal(fx.body("owner"))
			gatePolicy(t, testHandler, tc.list).configSyncOnPullRequest(context.Background(), body)
			want := 0
			if tc.allowed {
				want = 1
			}
			if fx.factory != want {
				t.Fatalf("config-sync GitHub readers built = %d, want %d", fx.factory, want)
			}
		})
	}
}

func TestGitHubGateReconcilerUnsignedScan(t *testing.T) {
	for _, tc := range gateCases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGitHubIntakeFixture(t)
			bindAccount(t, 2019001, tc.account)
			fixture.handler = gatePolicy(t, fixture.handler, tc.list)
			api := &fakeReconcileAPI{objects: unsignedObjects(fixture)}
			logs := captureLogs(t)
			recorded, err := reconcilerForTest(fixture, api).recordUnsigned(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.allowed {
				if recorded == 0 || api.objectCalls == 0 {
					t.Fatalf("allowed installation: recorded %d, GitHub calls %d", recorded, api.objectCalls)
				}
				return
			}
			if recorded != 0 || api.objectCalls != 0 {
				t.Fatalf("disallowed installation: recorded %d, GitHub calls %d, want 0 and 0", recorded, api.objectCalls)
			}
			if n := strings.Count(logs.String(), "denied by allowlist"); n != 1 {
				t.Fatalf("denied warnings = %d, want 1: %s", n, logs.String())
			}
		})
	}
}

func TestGitHubGateHeadRepair(t *testing.T) {
	for _, tc := range gateCases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGitHubReviewFixture(t)
			bindAccount(t, 2019001, tc.account)
			legacyUnverifiedPR(t, fixture)
			fixture.handler = gatePolicy(t, fixture.handler, tc.list)
			want := 0
			if tc.allowed {
				want = 1
			}
			if replayed := repairWithREST(t, fixture, map[string]any{"id": 424242, "full_name": "acme/widget", "fork": true}); replayed != want {
				t.Fatalf("replayed = %d, want %d", replayed, want)
			}
		})
	}
}

// Several disallowed rows of one installation cost one warning and no fetch.
func TestGitHubGateHeadRepairWarnsOncePerInstallation(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	bindAccount(t, 2019001, 7999)
	legacyUnverifiedPR(t, fixture)
	dbfx.Exec(t, `INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, head_repo_same)
		SELECT workspace_id, installation_id, repo_owner, repo_name, 2020, title, state, html_url, pr_created_at, pr_updated_at, head_sha, false
		FROM github_pull_request WHERE workspace_id=$1 AND pr_number=2019`, testWorkspaceID)
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM github_pull_request WHERE workspace_id=$1 AND pr_number=2020`, testWorkspaceID)
	})
	handler := gatePolicy(t, fixture.handler, "7001")
	handler.fetchPullRequestJSON = func(context.Context, int64, string, string, int32) (json.RawMessage, error) {
		t.Error("fetched a pull request of a disallowed installation")
		return nil, nil
	}
	logs := captureLogs(t)
	if _, err := handler.RepairUnverifiedHeadPullRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "denied by allowlist"); n != 1 {
		t.Fatalf("denied warnings = %d, want 1: %s", n, logs.String())
	}
}

// flakyBindings fails the installation-binding lookup while down is set.
type flakyBindings struct {
	db.DBTX
	down atomic.Bool
}

var errBindingsDown = errors.New("test: binding lookup down")

func (f *flakyBindings) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.down.Load() && strings.Contains(sql, "name: ListGitHubInstallationsByInstallationID") {
		return nil, errBindingsDown
	}
	return f.DBTX.Query(ctx, sql, args...)
}

func (f *flakyBindings) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return f.DBTX.Exec(ctx, sql, args...)
}

// flakyPolicy returns a policy copy of base whose binding lookups go through the flaky DB.
func flakyPolicy(t *testing.T, base *Handler, list string) (*Handler, *flakyBindings) {
	t.Helper()
	flaky := &flakyBindings{DBTX: testPool}
	h := gatePolicy(t, base, list)
	h.Queries = db.New(flaky)
	return h, flaky
}

// A failed policy lookup is not a denial: the delivery stays retryable and
// reaches routing once the lookup recovers.
func TestGitHubGateIntakeWorkerRetriesWhenPolicyLookupFails(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	bindAccount(t, 2019001, 7001)
	delivery := postIntakeForWorker(t, fixture, "-gate-lookup-down")
	worker := intakeWorkerForTest(t, fixture)
	var flaky *flakyBindings
	worker.handler, flaky = flakyPolicy(t, fixture.handler, "7001")
	flaky.down.Store(true)
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "deferred")
	assertIntakeWorkerDispatch(t, fixture, 0)
	if got := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1 AND reason='installation_not_allowed'`, testWorkspaceID); got != 0 {
		t.Fatalf("lookup failure settled %d revisions as denied", got)
	}
	flaky.down.Store(false)
	dbfx.Exec(t, `UPDATE github_webhook_intake SET next_attempt_at=now()-interval '1 second' WHERE delivery_guid=$1`, delivery)
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "dispatched")
	assertIntakeWorkerDispatch(t, fixture, 1)
}

func TestGitHubGateReconcilerReportsPolicyLookupFailure(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	bindAccount(t, 2019001, 7001)
	var flaky *flakyBindings
	fixture.handler, flaky = flakyPolicy(t, fixture.handler, "7001")
	flaky.down.Store(true)
	api := &fakeReconcileAPI{objects: unsignedObjects(fixture)}
	if _, err := reconcilerForTest(fixture, api).recordUnsigned(context.Background()); !errors.Is(err, errBindingsDown) {
		t.Fatalf("error = %v, want the lookup failure", err)
	}
	if api.objectCalls != 0 {
		t.Fatalf("GitHub calls = %d while the policy was unknown", api.objectCalls)
	}
}

func TestGitHubGateHeadRepairReportsPolicyLookupFailure(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	bindAccount(t, 2019001, 7001)
	legacyUnverifiedPR(t, fixture)
	handler, flaky := flakyPolicy(t, fixture.handler, "7001")
	flaky.down.Store(true)
	handler.fetchPullRequestJSON = func(context.Context, int64, string, string, int32) (json.RawMessage, error) {
		t.Error("fetched a pull request while the policy was unknown")
		return nil, nil
	}
	if _, err := handler.RepairUnverifiedHeadPullRequests(context.Background()); !errors.Is(err, errBindingsDown) {
		t.Fatalf("error = %v, want the lookup failure", err)
	}
}
