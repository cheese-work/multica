package handler

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type ciWatchEnv struct {
	ws, agent, repo string
}

// ciWatchAdminFixture is a workspace the test user owns, with the watcher flag on.
func ciWatchAdminFixture(t *testing.T, slug string) ciWatchEnv {
	t.Helper()
	withFeatureFlag(t, testHandler, featureflags.CIWatcher, true)
	ws, agent := delegatedWritesFixture(t, slug)
	dbfx.Cleanup(t, "DELETE FROM ci_watch_config_audit WHERE workspace_id=$1", ws)
	dbfx.Cleanup(t, "DELETE FROM ci_watch_config WHERE workspace_id=$1", ws)
	dbfx.Cleanup(t, "DELETE FROM ci_watch_action WHERE workspace_id=$1", ws)
	dbfx.Cleanup(t, "DELETE FROM ci_failure_event WHERE workspace_id=$1", ws)
	return ciWatchEnv{ws: ws, agent: agent, repo: "Acme/Widgets"}
}

func (e ciWatchEnv) path(suffix string) string {
	return "/api/workspaces/" + e.ws + "/ci-watch/repos/" + url.PathEscape(e.repo) + suffix
}

func (e ciWatchEnv) call(t *testing.T, h http.HandlerFunc, method, suffix string, body any, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParams(newRequest(method, e.path(suffix), body), "id", e.ws, "repo", url.PathEscape(e.repo))
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, h, req)
}

func (e ciWatchEnv) put(t *testing.T, body any) *testutil.Response {
	return e.call(t, testHandler.PutCIWatchConfig, "PUT", "", body, nil)
}

func (e ciWatchEnv) auditCount(t *testing.T) (n int) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT count(*) FROM ci_watch_config_audit WHERE workspace_id=$1 AND actor_type='member' AND actor_id=$2`, e.ws, testUserID).Scan(&n)
	return n
}

func TestCIWatchConfig_NonAdminWriteRefused(t *testing.T) {
	e := ciWatchAdminFixture(t, "ciw-member")
	dbfx.Exec(t, `UPDATE member SET role='member' WHERE workspace_id=$1 AND user_id=$2`, e.ws, testUserID)
	e.put(t, map[string]any{"mode": "act"}).Want(http.StatusForbidden)
	e.call(t, testHandler.GetCIWatchConfig, "GET", "", nil, nil).Want(http.StatusForbidden)
	e.call(t, testHandler.ListCIWatchEvents, "GET", "/events", nil, nil).Want(http.StatusForbidden)
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM ci_watch_config WHERE workspace_id=$1`, e.ws).Scan(&n)
	if n != 0 || e.auditCount(t) != 0 {
		t.Fatalf("refused write left rows: config=%d audit=%d", n, e.auditCount(t))
	}
}

func TestCIWatchConfig_MachineCredentialRefused(t *testing.T) {
	e := ciWatchAdminFixture(t, "ciw-token")
	for _, src := range []string{"task_token", "cloud_pat"} {
		e.call(t, testHandler.PutCIWatchConfig, "PUT", "", map[string]any{"mode": "act"},
			func(r *http.Request) { r.Header.Set("X-Actor-Source", src) }).Want(http.StatusForbidden)
	}
	if e.auditCount(t) != 0 {
		t.Fatal("machine write left an audit row")
	}
}

func TestCIWatchConfig_FlagOffReportsDisabled(t *testing.T) {
	e := ciWatchAdminFixture(t, "ciw-off")
	withFeatureFlag(t, testHandler, featureflags.CIWatcher, false)
	for name, res := range map[string]*testutil.Response{
		"get":    e.call(t, testHandler.GetCIWatchConfig, "GET", "", nil, nil),
		"put":    e.put(t, map[string]any{"mode": "act"}),
		"events": e.call(t, testHandler.ListCIWatchEvents, "GET", "/events", nil, nil),
	} {
		var out map[string]string
		res.Want(http.StatusForbidden).JSON(&out)
		if out["code"] != "ci_watcher_disabled" {
			t.Fatalf("%s: code = %q, want ci_watcher_disabled", name, out["code"])
		}
	}
	if e.auditCount(t) != 0 {
		t.Fatal("disabled server accepted a write")
	}
}

func TestCIWatchConfig_GetDefaultsThenPutRoundTrip(t *testing.T) {
	e := ciWatchAdminFixture(t, "ciw-roundtrip")
	var got map[string]any
	e.call(t, testHandler.GetCIWatchConfig, "GET", "", nil, nil).Want(http.StatusOK).JSON(&got)
	if got["repo"] != "acme/widgets" || got["mode"] != "off" || got["p1_mode"] != "shadow" {
		t.Fatalf("defaults = %v", got)
	}
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM ci_watch_config WHERE workspace_id=$1`, e.ws).Scan(&n)
	if n != 0 {
		t.Fatal("GET created a config row")
	}

	var put struct {
		Config  map[string]any `json:"config"`
		Changed []string       `json:"changed"`
	}
	e.put(t, map[string]any{"mode": "shadow", "branches": []string{"main"}}).Want(http.StatusOK).JSON(&put)
	if put.Config["mode"] != "shadow" || len(put.Changed) != 2 || put.Changed[0] != "branches" || put.Changed[1] != "mode" {
		t.Fatalf("put result = %+v", put)
	}
	e.call(t, testHandler.GetCIWatchConfig, "GET", "", nil, nil).Want(http.StatusOK).JSON(&got)
	if got["mode"] != "shadow" {
		t.Fatalf("get after put = %v", got)
	}
	if e.auditCount(t) != 2 {
		t.Fatalf("audit rows = %d, want 2", e.auditCount(t))
	}
	e.put(t, map[string]any{"mode": "shadow"}).Want(http.StatusOK)
	if e.auditCount(t) != 2 {
		t.Fatal("a no-op write added an audit row")
	}
}

func TestCIWatchConfig_PutValidation(t *testing.T) {
	e := ciWatchAdminFixture(t, "ciw-validate")
	bad := []map[string]any{
		{"mode": "loud"},
		{"unknown": 1},
		{},
		{"p1_mode": "off"},
		{"events": []string{"Push"}},
		{"max_new_issues_per_day": -1},
		{"fix_owner_kind": "squad"},
		{"fix_owner_kind": "agent", "fix_owner_id": uuid.NewString()}, // not in this workspace
		{"fix_owner_kind": "squad", "fix_owner_id": e.agent},          // an agent id is not a squad
		{"notify_member_id": uuid.NewString()},                        // not a member
		{"notify_member_id": "not-a-uuid"},
	}
	for _, body := range bad {
		var out map[string]string
		e.put(t, body).Want(http.StatusBadRequest).JSON(&out)
		if out["error"] == "" {
			t.Fatalf("body %v: no error message", body)
		}
	}
	e.repo = "not-a-repo"
	e.put(t, map[string]any{"mode": "act"}).Want(http.StatusBadRequest)
	if e.auditCount(t) != 0 {
		t.Fatalf("rejected writes left %d audit rows", e.auditCount(t))
	}

	e.repo = "acme/widgets"
	e.put(t, map[string]any{"fix_owner_kind": "agent", "fix_owner_id": e.agent, "notify_member_id": testUserID}).Want(http.StatusOK)
	e.put(t, map[string]any{"fix_owner_kind": nil, "fix_owner_id": nil, "notify_member_id": nil}).Want(http.StatusOK)
}

func TestCIWatchConfig_SwitchToShadowCancelsPending(t *testing.T) {
	e := ciWatchAdminFixture(t, "ciw-cancel")
	e.put(t, map[string]any{"mode": "act"}).Want(http.StatusOK)
	dbfx.Exec(t, `INSERT INTO ci_watch_action (workspace_id, repo_owner, repo_name, original_run_id, action_kind, protocol, state)
		VALUES ($1,'acme','widgets',1,'issue_create','P1','scheduled')`, e.ws)
	var out struct {
		Cancelled int `json:"cancelled"`
	}
	e.put(t, map[string]any{"mode": "shadow"}).Want(http.StatusOK).JSON(&out)
	var state string
	dbfx.QueryRow(t, `SELECT state||':'||reason FROM ci_watch_action WHERE workspace_id=$1`, e.ws).Scan(&state)
	if out.Cancelled != 1 || state != "cancelled:opted_out" {
		t.Fatalf("cancelled=%d state=%q, want 1 cancelled:opted_out", out.Cancelled, state)
	}
}

func TestCIWatchConfig_EventsNewestFirstAndLimit(t *testing.T) {
	e := ciWatchAdminFixture(t, "ciw-events")
	for i := 1; i <= 3; i++ {
		dbfx.Exec(t, `INSERT INTO ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt, workflow_name, head_branch, conclusion, created_at)
			VALUES ($1,'acme','widgets',$2::bigint,1,'ci','main','failure', now() - (10 - $2::bigint) * interval '1 minute')`, e.ws, i)
	}
	var out struct {
		Events []map[string]any `json:"events"`
	}
	e.call(t, testHandler.ListCIWatchEvents, "GET", "/events?limit=2", nil, nil).Want(http.StatusOK).JSON(&out)
	if len(out.Events) != 2 || out.Events[0]["run_id"].(float64) != 3 {
		t.Fatalf("events = %v, want 2 newest first", out.Events)
	}
	for _, q := range []string{"?limit=0", "?limit=abc", "?limit=1000"} {
		e.call(t, testHandler.ListCIWatchEvents, "GET", "/events"+q, nil, nil).Want(http.StatusBadRequest)
	}
	e.call(t, testHandler.ListCIWatchEvents, "GET", "/events", nil, nil).Want(http.StatusOK).JSON(&out)
	if len(out.Events) != 3 {
		t.Fatalf("default limit returned %d events, want 3", len(out.Events))
	}
}

func TestCIWatchConfig_EventsCarryActionFields(t *testing.T) {
	e := ciWatchAdminFixture(t, "ciw-events-enriched")
	dbfx.Exec(t, `INSERT INTO ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt, workflow_name, head_branch, event, conclusion)
		VALUES ($1,'acme','widgets',77,1,'ci','main','push','failure')`, e.ws)
	dbfx.Exec(t, `INSERT INTO ci_watch_action (workspace_id, repo_owner, repo_name, original_run_id, action_kind, state, protocol)
		VALUES ($1,'acme','widgets',77,'rerun','done','P1')`, e.ws)
	var out struct {
		Events []map[string]any `json:"events"`
	}
	e.call(t, testHandler.ListCIWatchEvents, "GET", "/events", nil, nil).Want(http.StatusOK).JSON(&out)
	if len(out.Events) != 1 {
		t.Fatalf("events = %v", out.Events)
	}
	// class and outcome come from system-1 observations (covered in ciwatch);
	// the keys must still be present so the panel can rely on them.
	want := map[string]string{"event": "push", "class": "", "protocol": "P1", "action": "rerun", "state": "done", "outcome": ""}
	for k, v := range want {
		if got, ok := out.Events[0][k]; !ok || got != v {
			t.Errorf("event[%q] = %v (present=%v), want %q (event = %v)", k, got, ok, v, out.Events[0])
		}
	}
}
