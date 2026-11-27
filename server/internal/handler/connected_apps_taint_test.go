package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9518 policy B: a tainted or downgraded run cannot make mutating or
// destructive Composio calls, even when the app grant allows them. Reads stay
// governed by the app policy.

// restrictions are the ways a run stops being trusted. Each one alone must
// close the write path.
var appsRunRestrictions = []struct {
	name  string
	apply func(t *testing.T, f *appsFixture)
}{
	{"task taint", func(t *testing.T, f *appsFixture) {
		dbfx.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['t1'] WHERE id = $1`, f.taskID)
	}},
	{"issue taint", func(t *testing.T, f *appsFixture) {
		dbfx.Exec(t, `SELECT issue_taint_note($1::uuid, ARRAY['t1'])`, f.issueID)
	}},
	{"downgrade fencing", func(t *testing.T, f *appsFixture) { f.elevation(t, "fencing") }},
	{"downgrade delivered", func(t *testing.T, f *appsFixture) { f.elevation(t, "restricted_final") }},
}

// elevation records the task's elevation row in the given state, with no taint
// on the task, so only the downgrade signal is under test.
func (f *appsFixture) elevation(t *testing.T, state string) {
	t.Helper()
	dbfx.InsertNoID(t, "task_elevation", testutil.Cols{
		"task_id": f.taskID, "workspace_id": testWorkspaceID, "issue_id": f.issueID,
		"grant_ids": testutil.Raw("ARRAY[gen_random_uuid()]"), "scopes": testutil.Raw("ARRAY['docker']"), "state": state,
	}, "task_id = $1", f.taskID)
}

// writeApp attaches Gmail at write level on the personal account.
func (f *appsFixture) writeApp(t *testing.T) string {
	t.Helper()
	f.executionAccount(t, "personal")
	if w := f.putAgentApps(t, testUserID, []map[string]string{{"toolkit_slug": "gmail", "account_scope": "personal", "level": "write"}}); w.Code != http.StatusOK {
		t.Fatalf("attach app: %d %s", w.Code, w.Body.String())
	}
	return f.token(t)
}

func (f *appsFixture) wantDenied(t *testing.T, token, tool string) {
	t.Helper()
	before := len(f.sdk.executions())
	_, text, isErr := f.gateway(t, token, "apps_run_tool", map[string]any{"tool": tool, "arguments": map[string]any{"user_id": "me"}})
	if !isErr || !strings.Contains(text, "blocked") {
		t.Fatalf("%s: want a blocked error, got error=%v %q", tool, isErr, text)
	}
	if got := len(f.sdk.executions()); got != before {
		t.Fatalf("%s executed %d time(s) for a restricted run", tool, got-before)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM app_tool_call WHERE task_id=$1 AND tool_slug=$2 AND outcome='blocked'`, f.taskID, tool); n != 1 {
		t.Fatalf("%s: audited blocked calls = %d, want 1", tool, n)
	}
}

func (f *appsFixture) wantRuns(t *testing.T, token, tool string) {
	t.Helper()
	before := len(f.sdk.executions())
	_, text, isErr := f.gateway(t, token, "apps_run_tool", map[string]any{"tool": tool, "arguments": map[string]any{"user_id": "me"}})
	if isErr || !strings.Contains(text, tool) {
		t.Fatalf("%s: want a result, got error=%v %q", tool, isErr, text)
	}
	if got := len(f.sdk.executions()); got != before+1 {
		t.Fatalf("%s executions = %d, want %d", tool, got, before+1)
	}
}

// (a) tainted run, mutating call, app grant: denied. (b) downgraded: the same.
func TestConnectedApps_RestrictedRunCannotMutate(t *testing.T) {
	for _, r := range appsRunRestrictions {
		t.Run(r.name, func(t *testing.T) {
			f := newAppsFixture(t)
			token := f.writeApp(t)
			r.apply(t, f)
			f.wantDenied(t, token, "GMAIL_SEND_EMAIL")
			f.wantDenied(t, token, "GMAIL_DELETE_MESSAGE")
		})
	}
}

// (c) reads still follow the app policy.
func TestConnectedApps_RestrictedRunReadsFollowAppPolicy(t *testing.T) {
	for _, r := range appsRunRestrictions {
		t.Run(r.name, func(t *testing.T) {
			f := newAppsFixture(t)
			token := f.writeApp(t)
			r.apply(t, f)
			f.wantRuns(t, token, "GMAIL_FETCH_EMAILS")

			// The app policy still wins: a blocked read class stays blocked.
			f.h.UpdateAppRules(httptest.NewRecorder(), appsReqAs(t, testUserID, http.MethodPut, "/api/apps/rules?toolkit_slug=gmail&scope=personal", map[string]any{
				"rules": []map[string]string{{"action_class": "read", "policy": "block"}},
			}))
			f.wantDenied(t, token, "GMAIL_FETCH_EMAILS")
		})
	}
}

// (d) an untrusted-free run is unchanged, including an elevated one.
func TestConnectedApps_UntaintedRunUnchanged(t *testing.T) {
	f := newAppsFixture(t)
	token := f.writeApp(t)
	f.wantRuns(t, token, "GMAIL_SEND_EMAIL")
	f.wantRuns(t, token, "GMAIL_DELETE_MESSAGE")
	f.wantRuns(t, token, "GMAIL_FETCH_EMAILS")
}

// An elevated run is trusted: only a downgrade or taint closes the write path.
func TestConnectedApps_ElevatedRunUnchanged(t *testing.T) {
	f := newAppsFixture(t)
	token := f.writeApp(t)
	f.elevation(t, "elevated")
	f.wantRuns(t, token, "GMAIL_SEND_EMAIL")
}

// The state is read when the call executes, not when the gateway loaded the
// run: a taint that lands in between still stops the write.
func TestConnectedApps_TaintArrivingMidCallStopsWrite(t *testing.T) {
	f := newAppsFixture(t)
	token := f.writeApp(t)
	f.sdk.beforeListTools = func() {
		dbfx.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['t1'] WHERE id = $1`, f.taskID)
		f.sdk.beforeListTools = nil
	}
	f.wantDenied(t, token, "GMAIL_SEND_EMAIL")
}
