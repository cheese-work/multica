package handler

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Workflow step entry gates (CHE-2531, plan docs/plans/CHE-2295-entry-gates.md).

// gateSteps is a workflow whose in_review step carries the given entry gates.
func gateSteps(entry ...map[string]any) []map[string]any {
	return []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "todo"},
		{"status_key": "in_progress"},
		{"status_key": "in_review", "entry": entry},
		{"status_key": "blocked"},
	}
}

func gate(guard, mode string) map[string]any {
	return map[string]any{"guard": guard, "mode": mode, "member_override": true}
}

// asAgentTask makes req an agent task-token call, as the CLI makes in a run.
func asAgentTask(t *testing.T, req *http.Request) *http.Request {
	t.Helper()
	agentID := handlerTestAgentID(t)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", createHandlerTestTaskForAgent(t, agentID))
	return req
}

func asSource(source string) func(*http.Request) *http.Request {
	return func(req *http.Request) *http.Request {
		req.Header.Set("X-Actor-Source", source)
		return req
	}
}

// putIssue is a person's PUT unless an actor option changes who calls.
func putIssue(t *testing.T, issueID string, body map[string]any, actor ...func(*http.Request) *http.Request) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("PUT", "/api/issues/"+issueID, body), "id", issueID)
	for _, as := range actor {
		req = as(req)
	}
	return testutil.Call(t, testHandler.UpdateIssue, req)
}

// gatePR is one PR of the issue's child, as the stored facts read it.
type gatePR struct{ state, rollup, review string }

var (
	openRed     = gatePR{state: "open", rollup: "FAILURE"}
	openGreen   = gatePR{state: "open", rollup: "SUCCESS"}
	draftGreen  = gatePR{state: "draft", rollup: "SUCCESS"}
	draftPR     = gatePR{state: "draft"}
	mergedPR    = gatePR{state: "merged"}
	closedPR    = gatePR{state: "closed"}
	gateInstall = int64(2531000)
)

// linkGatePRs links the PRs to the child of c, with a checks snapshot and a
// review on each PR's current head.
func linkGatePRs(t *testing.T, c edgeCase, installationID int64, prs ...gatePR) {
	t.Helper()
	for i, p := range prs {
		number := int32(installationID%100000)*10 + int32(i)
		linkedPRForWakeup(t, c.fx, installationID, number, fmt.Sprintf("gate-%d-%d", installationID, i))
		dbfx.Exec(t, `UPDATE github_pull_request SET state=$3, checks_rollup_state=NULLIF($4,''), snapshot_head_sha=head_sha,
			review_state=$5, review_head_sha=head_sha WHERE installation_id=$1 AND pr_number=$2`,
			installationID, number, p.state, p.rollup, p.review)
	}
}

func failedGuards(t *testing.T, res *testutil.Response) []string {
	t.Helper()
	var out []string
	raw, _ := res.Map()["failed_guards"].([]any)
	for _, item := range raw {
		out = append(out, item.(map[string]any)["guard"].(string))
	}
	return out
}

func activityCount(t *testing.T, issueID, action string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action=$2`, issueID, action)
}

// An agent writing in_review with a draft PR on a block gate is refused. Red
// before the engine existed: the step dropped `entry` and the write succeeded.
func TestEntryGateBlocksAgentWriteWithDraftPR(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2531001
	c := newEdgeCase(t, installationID, gateSteps(gate("pr.ready", "block")))
	linkedPRForWakeup(t, c.fx, installationID, 253101, "gate-a")
	setChildPR(t, c.fx.child.ID, "draft")
	c.enter(t, "in_progress")

	req := asAgentTask(t, withURLParam(newRequest("PUT", "/api/issues/"+c.issueID, map[string]any{"status": "in_review"}), "id", c.issueID))
	res := testutil.Call(t, testHandler.UpdateIssue, req)
	res.Want(http.StatusConflict)
	if code, _ := res.Map()["code"].(string); code != "entry_gate_failed" {
		t.Fatalf("code = %q, want entry_gate_failed; body %s", code, res.Text())
	}
	wantIssueStatus(t, c.issueID, "in_progress")
}

// The plan's expected-results table, one row per case.
func TestEntryGateExpectedResults(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	rows := []struct {
		name         string
		prs          []gatePR
		childrenDone bool
		gates        []map[string]any
		wantFailed   []string // nil: the write passes
	}{
		{"merged PR, pr.merged", []gatePR{mergedPR}, false, []map[string]any{gate("pr.merged", "block")}, nil},
		{"merged PR, pr.ready", []gatePR{mergedPR}, false, []map[string]any{gate("pr.ready", "block")}, []string{"pr.ready"}},
		{"closed PR, pr.closed_unmerged", []gatePR{closedPR}, false, []map[string]any{gate("pr.closed_unmerged", "block")}, nil},
		{"closed PR, pr.merged", []gatePR{closedPR}, false, []map[string]any{gate("pr.merged", "block")}, []string{"pr.merged"}},
		{"no PR, open-PR family", nil, false, []map[string]any{gate("pr.ready", "block")}, []string{"pr.ready"}},
		{"no PR, terminal family", nil, false, []map[string]any{gate("pr.merged", "block")}, []string{"pr.merged"}},
		{"red open + green draft, ready + checks_green", []gatePR{openRed, draftGreen}, false,
			[]map[string]any{gate("pr.ready", "block"), gate("pr.checks_green", "block")}, []string{"pr.ready", "pr.checks_green"}},
		{"red open + green draft, checks_green", []gatePR{openRed, draftGreen}, false,
			[]map[string]any{gate("pr.checks_green", "block")}, []string{"pr.checks_green"}},
		{"green open, children done", []gatePR{openGreen}, true,
			[]map[string]any{gate("pr.ready", "block"), gate("pr.checks_green", "block"), gate("children_done", "block")}, nil},
		{"merged, children not done", []gatePR{mergedPR}, false,
			[]map[string]any{gate("pr.merged", "block"), gate("children_done", "block")}, []string{"children_done"}},
		{"merged, merged block + ready warn", []gatePR{mergedPR}, false,
			[]map[string]any{gate("pr.merged", "block"), gate("pr.ready", "warn")}, nil},
		{"merged, merged block + ready block", []gatePR{mergedPR}, false,
			[]map[string]any{gate("pr.merged", "block"), gate("pr.ready", "block")}, []string{"pr.ready"}},
	}
	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			installationID := gateInstall + 100 + int64(i)
			c := newEdgeCase(t, installationID, gateSteps(row.gates...))
			linkGatePRs(t, c, installationID, row.prs...)
			if row.childrenDone {
				dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, c.fx.child.ID)
			}
			c.enter(t, "in_progress")
			res := putIssue(t, c.issueID, map[string]any{"status": "in_review"})
			if row.wantFailed == nil {
				res.Want(http.StatusOK)
				wantIssueStatus(t, c.issueID, "in_review")
				return
			}
			res.Want(http.StatusConflict)
			if got := failedGuards(t, res); !reflect.DeepEqual(got, row.wantFailed) {
				t.Fatalf("failed guards = %v, want %v; body %s", got, row.wantFailed, res.Text())
			}
			wantIssueStatus(t, c.issueID, "in_progress")
		})
	}
}

// A warn gate lets the write through and says so: one activity row per
// (issue, step, head), one comment for the first warning of the step.
func TestEntryGateWarnRecordsOncePerHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	installationID := gateInstall + 200
	c := newEdgeCase(t, installationID, gateSteps(gate("pr.ready", "warn")))
	linkGatePRs(t, c, installationID, draftPR)
	warnings := func() (rows, comments int) {
		return activityCount(t, c.issueID, "entry_gate_warning"),
			dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND content LIKE 'Entry gate warning%'`, c.issueID)
	}
	enterReview := func() {
		t.Helper()
		c.enter(t, "in_progress")
		putIssue(t, c.issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)
	}
	enterReview()
	if rows, comments := warnings(); rows != 1 || comments != 1 {
		t.Fatalf("first warning: %d activity rows, %d comments, want 1 and 1", rows, comments)
	}
	enterReview()
	if rows, comments := warnings(); rows != 1 || comments != 1 {
		t.Fatalf("same head again: %d activity rows, %d comments, want 1 and 1", rows, comments)
	}
	dbfx.Exec(t, `UPDATE github_pull_request SET head_sha='gate-new-head', snapshot_head_sha='gate-new-head' WHERE installation_id=$1`, installationID)
	enterReview()
	if rows, comments := warnings(); rows != 2 || comments != 1 {
		t.Fatalf("new head: %d activity rows, %d comments, want 2 and 1", rows, comments)
	}
}

// Override: only a person's own credential, with a reason, on gates that allow it.
func TestEntryGateOverrideCredentials(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const reason = "reviewed by hand"
	cases := []struct {
		name     string
		actor    func(*testing.T, *http.Request) *http.Request
		reason   string
		override bool // member_override of the gate
		want     int
	}{
		{"agent task token", func(t *testing.T, r *http.Request) *http.Request { return asAgentTask(t, r) }, reason, true, http.StatusConflict},
		{"cloud PAT", func(_ *testing.T, r *http.Request) *http.Request { return asSource("cloud_pat")(r) }, reason, true, http.StatusConflict},
		{"interactive JWT", func(_ *testing.T, r *http.Request) *http.Request { return asSource("interactive_jwt")(r) }, reason, true, http.StatusOK},
		{"CLI JWT", func(_ *testing.T, r *http.Request) *http.Request { return asSource("cli_jwt")(r) }, reason, true, http.StatusOK},
		{"PAT", func(_ *testing.T, r *http.Request) *http.Request { return asSource("pat")(r) }, reason, true, http.StatusOK},
		{"person, gate member_override=false", func(_ *testing.T, r *http.Request) *http.Request { return r }, reason, false, http.StatusConflict},
		{"person, no reason", func(_ *testing.T, r *http.Request) *http.Request { return r }, "", true, http.StatusConflict},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installationID := gateInstall + 300 + int64(i)
			g := gate("pr.ready", "block")
			g["member_override"] = tc.override
			c := newEdgeCase(t, installationID, gateSteps(g))
			c.enter(t, "in_progress")
			body := map[string]any{"status": "in_review"}
			if tc.reason != "" {
				body["entry_gate_override_reason"] = tc.reason
			}
			req := tc.actor(t, withURLParam(newRequest("PUT", "/api/issues/"+c.issueID, body), "id", c.issueID))
			testutil.Call(t, testHandler.UpdateIssue, req).Want(tc.want)
			if tc.want == http.StatusOK {
				var got string
				dbfx.QueryRow(t, `SELECT details->>'reason' FROM activity_log WHERE issue_id=$1 AND action='entry_gate_override'`, c.issueID).Scan(&got)
				if got != reason {
					t.Fatalf("override activity reason = %q, want %q", got, reason)
				}
				return
			}
			wantIssueStatus(t, c.issueID, "in_progress")
			if n := activityCount(t, c.issueID, "entry_gate_override"); n != 0 {
				t.Fatalf("a refused override left %d activity rows", n)
			}
		})
	}
	// A machine credential cannot gain the human row by forging X-Actor-Source:
	// the auth middleware drops a client-supplied value and stamps the real one
	// (middleware/auth_provenance_test.go, daemon_auth_test.go).
}

func TestEntryGateOverrideReasonLength(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, gateInstall+400, gateSteps(gate("pr.ready", "block")))
	c.enter(t, "in_progress")
	for _, reason := range []string{" ", string(make([]rune, 501))} {
		putIssue(t, c.issueID, map[string]any{"status": "in_review", "entry_gate_override_reason": reason}).Want(http.StatusBadRequest)
	}
}

// Gates are part of what a bound workflow write changes: removing one is a
// steps change, never an instructions-only one.
func TestEntryChangeIsAStepsWrite(t *testing.T) {
	before := []issueworkflow.Step{{StatusKey: "in_review", Entry: []issueworkflow.EntryGate{{Guard: "pr.ready", Mode: "block"}}}}
	after := []issueworkflow.Step{{StatusKey: "in_review"}}
	if got := workflowWriteField(before, after); got != workflowStepsField {
		t.Fatalf("dropping a gate classified as %q, want %q", got, workflowStepsField)
	}
}
