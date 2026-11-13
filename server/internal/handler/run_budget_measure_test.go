package handler

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// scripts/run-budget-measure.sql holds the three before/after queries of the
// run-budget plan (CHE-5236 "Measure"): M1 blocked-by-budget issues, M2 budget
// asks, M3 runs per merged PR. Each query sits under a `-- @query Mn` line, so
// the test runs one at a time with the psql variables filled in.
func measureQuery(test *testing.T, name string, vars map[string]string) []map[string]any {
	test.Helper()
	file, err := os.ReadFile("../../../scripts/run-budget-measure.sql")
	if err != nil {
		test.Fatalf("read query file: %v", err)
	}
	var body string
	marks := regexp.MustCompile(`(?m)^-- @query (\w+)\s*$`).FindAllStringSubmatchIndex(string(file), -1)
	for i, mark := range marks {
		if string(file)[mark[2]:mark[3]] != name {
			continue
		}
		end := len(file)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		body = string(file)[mark[1]:end]
	}
	if strings.TrimSpace(body) == "" {
		test.Fatalf("query %s not found in the file", name)
	}
	for key, value := range vars {
		body = strings.ReplaceAll(body, ":'"+key+"'", "'"+value+"'")
	}
	if strings.Contains(body, ":'") {
		test.Fatalf("query %s has an unbound psql variable:\n%s", name, body)
	}
	body = strings.TrimRight(strings.TrimSpace(body), ";")

	var raw []byte
	dbfx.QueryRow(test, fmt.Sprintf(`SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY t.day), '[]'::jsonb)
		FROM (%s) t`, body)).Scan(&raw)
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		test.Fatalf("decode %s: %v", name, err)
	}
	return rows
}

func byDay(test *testing.T, rows []map[string]any) map[string][]map[string]any {
	test.Helper()
	out := map[string][]map[string]any{}
	for _, row := range rows {
		day, _ := row["day"].(string)
		out[day] = append(out[day], row)
	}
	return out
}

func ictTime(test *testing.T, text string) time.Time {
	test.Helper()
	at, err := time.Parse("2006-01-02 15:04", text)
	if err != nil {
		test.Fatal(err)
	}
	return at.Add(-7 * time.Hour)
}

// number reads a JSON number; ok is false for null or a missing key.
func number(row map[string]any, key string) (float64, bool) {
	value, ok := row[key].(float64)
	return value, ok
}

func wantNumber(test *testing.T, label string, row map[string]any, key string, want float64) {
	test.Helper()
	if got, ok := number(row, key); !ok || got != want {
		test.Errorf("%s %s = %v, want %v", label, key, row[key], want)
	}
}

func wantNull(test *testing.T, label string, row map[string]any, key string) {
	test.Helper()
	if row[key] != nil {
		test.Errorf("%s %s = %v, want null (unavailable, never 0)", label, key, row[key])
	}
}

type measureFixture struct {
	fx   *testutil.Fixture
	vars map[string]string
}

func newMeasureFixture(test *testing.T) measureFixture {
	test.Helper()
	slug := fmt.Sprintf("measure-%d", time.Now().UnixNano())
	workspaceID := dbfx.Workspace(test, "measure", slug)
	return measureFixture{
		fx: testutil.New(testPool, workspaceID, testUserID),
		vars: map[string]string{
			"workspace_id": workspaceID,
			"from_day":     "2026-10-28",
			"to_day":       "2026-11-03",
			"report_time":  "2026-11-02 12:00:00+07",
		},
	}
}

func (m measureFixture) activity(test *testing.T, issueID, actorType, action, details string, at time.Time) {
	test.Helper()
	cols := testutil.Cols{
		"workspace_id": m.fx.WorkspaceID, "issue_id": issueID, "actor_type": actorType,
		"action": action, "details": details, "created_at": at,
	}
	m.fx.Insert(test, "activity_log", cols)
}

func (m measureFixture) blocked(test *testing.T, issueID string, cause string, at time.Time) {
	test.Helper()
	details := `{"from":"in_progress","to":"blocked"}`
	if cause != "" {
		details = fmt.Sprintf(`{"from":"in_progress","to":"blocked","trigger":{"cause":{"kind":%q}}}`, cause)
	}
	m.activity(test, issueID, "system", "status_changed", details, at)
}

func (m measureFixture) unblocked(test *testing.T, issueID string, at time.Time) {
	test.Helper()
	m.activity(test, issueID, "member", "status_changed", `{"from":"blocked","to":"in_progress"}`, at)
}

func TestRunBudgetMeasureM1BlockedByBudget(test *testing.T) {
	m := newMeasureFixture(test)
	a := m.fx.Issue(test, "blocked twice")
	b := m.fx.Issue(test, "still blocked")
	c := m.fx.Issue(test, "other cause")

	// Issue a: two budget blocks on ICT 31 Oct. 20:00Z is 03:00 ICT. The first
	// ends after 3 h, the second after 2 h.
	m.blocked(test, a, "run_budget", ictTime(test, "2026-10-31 03:00"))
	m.unblocked(test, a, ictTime(test, "2026-10-31 06:00"))
	m.blocked(test, a, "run_budget", ictTime(test, "2026-10-31 08:00"))
	m.unblocked(test, a, ictTime(test, "2026-10-31 10:00"))
	// Issue b: blocked on 1 Nov at 09:00 ICT, never left: counts to the report
	// time, 2 Nov 12:00 ICT, which is 27 h.
	m.blocked(test, b, "run_budget", ictTime(test, "2026-11-01 09:00"))
	// Issue c: blocked for another cause, and one with no trigger at all.
	m.blocked(test, c, "approval", ictTime(test, "2026-11-01 10:00"))
	m.blocked(test, c, "", ictTime(test, "2026-11-01 11:00"))
	// A move out of blocked on another issue must not close b.
	m.unblocked(test, c, ictTime(test, "2026-11-01 12:00"))

	// Exhausted notices cross-check the blocks. A `blocked` notice is ignored.
	for _, notice := range []struct{ issue, window, kind, at string }{
		{a, "w1", "exhausted", "2026-10-31 03:00"},
		{b, "w2", "exhausted", "2026-11-01 09:00"},
		{b, "w2", "blocked", "2026-11-01 09:00"},
	} {
		m.fx.Insert(test, "run_budget_notice", testutil.Cols{
			"id": testutil.Raw("gen_random_uuid()"), "workspace_id": m.fx.WorkspaceID, "issue_id": notice.issue, "window_id": notice.window,
			"kind": notice.kind, "created_at": ictTime(test, notice.at),
		})
	}

	days := byDay(test, measureQuery(test, "M1", m.vars))
	if len(days) != 7 {
		test.Fatalf("M1 returned %d days, want the 7 of the window", len(days))
	}
	for _, day := range []string{"2026-10-28", "2026-10-29", "2026-10-30"} {
		row := days[day][0]
		if row["covered"] != false {
			test.Errorf("%s covered = %v, want false: before the first run_budget row", day, row["covered"])
		}
		wantNull(test, day, row, "blocked_issues")
		wantNull(test, day, row, "blocked_hours")
	}
	d31 := days["2026-10-31"][0]
	wantNumber(test, "10-31", d31, "blocked_issues", 1)
	wantNumber(test, "10-31", d31, "blocked_occurrences", 2)
	wantNumber(test, "10-31", d31, "blocked_hours", 5)
	wantNumber(test, "10-31", d31, "exhausted_notice_issues", 1)
	d01 := days["2026-11-01"][0]
	wantNumber(test, "11-01", d01, "blocked_issues", 1)
	wantNumber(test, "11-01", d01, "blocked_hours", 27)
	wantNumber(test, "11-01", d01, "exhausted_notice_issues", 1)
	d02 := days["2026-11-02"][0]
	if d02["covered"] != true {
		test.Errorf("11-02 covered = %v, want true", d02["covered"])
	}
	wantNumber(test, "11-02", d02, "blocked_issues", 0)
	wantNumber(test, "11-02", d02, "blocked_hours", 0)
}

func TestRunBudgetMeasureM1NoSourceRowsIsUnavailable(test *testing.T) {
	m := newMeasureFixture(test)
	days := byDay(test, measureQuery(test, "M1", m.vars))
	if len(days) != 7 {
		test.Fatalf("M1 returned %d days, want 7", len(days))
	}
	for day, rows := range days {
		if rows[0]["covered"] != false {
			test.Errorf("%s covered = %v, want false: no run_budget row exists", day, rows[0]["covered"])
		}
		wantNull(test, day, rows[0], "blocked_issues")
	}
}

func TestRunBudgetMeasureM1MarksTheChangeDate(test *testing.T) {
	m := newMeasureFixture(test)
	m.vars["from_day"], m.vars["to_day"] = "2026-11-12", "2026-11-14"
	days := byDay(test, measureQuery(test, "M1", m.vars))
	if len(days) != 3 {
		test.Fatalf("M1 returned %d days, want 3", len(days))
	}
	if days["2026-11-13"][0]["mark"] != "CHE-5303 change date" {
		test.Errorf("13 Nov mark = %v", days["2026-11-13"][0]["mark"])
	}
	if days["2026-11-12"][0]["mark"] != nil {
		test.Errorf("12 Nov mark = %v, want none", days["2026-11-12"][0]["mark"])
	}
}

func (m measureFixture) action(test *testing.T, actionType, state string, createdAt time.Time, executedAt *time.Time, passkey bool) {
	test.Helper()
	cols := testutil.Cols{
		"workspace_id": m.fx.WorkspaceID, "decision_request_id": testutil.Raw("gen_random_uuid()"),
		"action_type": actionType, "action_version": 1, "params": "{}",
		"payload_sha256":        strings.Repeat("a", 64),
		"expires_at":            createdAt.Add(24 * time.Hour),
		"state":                 state,
		"requested_by_agent_id": testutil.Raw("gen_random_uuid()"),
		"created_at":            createdAt,
	}
	if executedAt != nil {
		cols["executed_at"] = *executedAt
	}
	if passkey {
		cols["approved_by_user_id"] = testUserID
		cols["webauthn_credential_id"] = testutil.Raw("gen_random_uuid()")
	}
	m.fx.Insert(test, "decision_action", cols)
}

func TestRunBudgetMeasureM2BudgetAsks(test *testing.T) {
	m := newMeasureFixture(test)
	issue := m.fx.Issue(test, "asked")
	at := func(text string) time.Time { return ictTime(test, text) }
	exec := func(text string) *time.Time { value := at(text); return &value }

	// Asked on 31 Oct ICT, executed on 1 Nov ICT with a passkey.
	m.action(test, "issue.run_budget.top_up", "executed", at("2026-10-31 20:00"), exec("2026-11-01 09:00"), true)
	m.action(test, "issue.run_budget.top_up", "pending", at("2026-10-31 21:00"), nil, false)
	m.action(test, "issue.run_budget.raise", "failed", at("2026-10-31 22:00"), exec("2026-10-31 23:00"), true)
	m.action(test, "workflow.budget.set", "executed", at("2026-11-01 10:00"), exec("2026-11-01 11:00"), true)
	m.action(test, "decision.other", "executed", at("2026-11-01 10:00"), exec("2026-11-01 11:00"), true)

	// Origin is known from 1 Nov, the first run_budget_ask row. System is the
	// CHE-5303 server card.
	m.activity(test, issue, "agent", "run_budget_ask", `{"action_type":"issue.run_budget.top_up","window_id":"w"}`, at("2026-11-01 08:00"))
	m.activity(test, issue, "agent", "run_budget_ask", `{"action_type":"issue.run_budget.top_up","window_id":"w"}`, at("2026-11-01 08:30"))
	m.activity(test, issue, "member", "run_budget_ask", `{"action_type":"issue.run_budget.top_up","window_id":"w"}`, at("2026-11-01 09:00"))
	m.activity(test, issue, "system", "run_budget_ask", `{"action_type":"workflow.budget.set","window_id":"w"}`, at("2026-11-01 10:00"))
	// The auto-approver writes no decision_action: its approval is an activity.
	m.activity(test, issue, "system", "run_budget_top_up", `{"auto":"auto-approver v1","runs":5}`, at("2026-11-02 09:00"))
	m.activity(test, issue, "member", "run_budget_top_up", `{"action_id":"x"}`, at("2026-11-02 09:30"))

	days := byDay(test, measureQuery(test, "M2", m.vars))
	if len(days) != 7 {
		test.Fatalf("M2 returned %d days, want 7", len(days))
	}
	row := func(day, actionType string) map[string]any {
		for _, r := range days[day] {
			if r["action_type"] == actionType {
				return r
			}
		}
		test.Fatalf("no M2 row for %s %s", day, actionType)
		return nil
	}
	if len(days["2026-11-01"]) != 4 {
		test.Fatalf("M2 gives %d rows a day, want one per action type (4)", len(days["2026-11-01"]))
	}

	// Before the first ask row of the workspace: unavailable, not 0.
	before := row("2026-10-30", "issue.run_budget.top_up")
	wantNull(test, "10-30", before, "asks")
	wantNull(test, "10-30", before, "executed")

	topUp31 := row("2026-10-31", "issue.run_budget.top_up")
	wantNumber(test, "10-31 top_up", topUp31, "asks", 2)
	wantNumber(test, "10-31 top_up", topUp31, "executed", 0)
	if topUp31["origin_available"] != false {
		test.Errorf("10-31 origin_available = %v, want false", topUp31["origin_available"])
	}
	wantNull(test, "10-31 top_up", topUp31, "asks_by_agent")
	wantNumber(test, "10-31 raise", row("2026-10-31", "issue.run_budget.raise"), "asks", 1)
	// A failed action is an ask, not an executed ask.
	wantNumber(test, "10-31 raise", row("2026-10-31", "issue.run_budget.raise"), "executed", 0)

	topUp01 := row("2026-11-01", "issue.run_budget.top_up")
	wantNumber(test, "11-01 top_up", topUp01, "asks", 0)
	wantNumber(test, "11-01 top_up", topUp01, "executed", 1)
	wantNumber(test, "11-01 top_up", topUp01, "executed_by_passkey", 1)
	wantNumber(test, "11-01 top_up", topUp01, "asks_by_agent", 2)
	wantNumber(test, "11-01 top_up", topUp01, "asks_by_member", 1)
	wantNumber(test, "11-01 top_up", topUp01, "asks_by_system", 0)
	wfs := row("2026-11-01", "workflow.budget.set")
	wantNumber(test, "11-01 workflow.budget.set", wfs, "asks", 1)
	wantNumber(test, "11-01 workflow.budget.set", wfs, "executed", 1)
	wantNumber(test, "11-01 workflow.budget.set", wfs, "asks_by_system", 1)
	wantNumber(test, "11-01 workflow.budget.set", wfs, "asks_by_agent", 0)

	// The auto-approver column: top-ups only, and only the rows with `auto`.
	wantNumber(test, "11-02 top_up", row("2026-11-02", "issue.run_budget.top_up"), "executed_by_auto", 1)
	wantNumber(test, "11-02 raise", row("2026-11-02", "issue.run_budget.raise"), "executed_by_auto", 0)
	// No auto row before 2 Nov: the auto-approver was not running, so unavailable.
	wantNull(test, "11-01 top_up", topUp01, "executed_by_auto")
}

func TestRunBudgetMeasureM2NoRowsIsUnavailable(test *testing.T) {
	m := newMeasureFixture(test)
	days := byDay(test, measureQuery(test, "M2", m.vars))
	if len(days) != 7 {
		test.Fatalf("M2 returned %d days, want 7", len(days))
	}
	for day, rows := range days {
		for _, r := range rows {
			wantNull(test, day, r, "asks")
			wantNull(test, day, r, "executed")
			if r["origin_available"] != false {
				test.Errorf("%s origin_available = %v, want false", day, r["origin_available"])
			}
		}
	}
}

func (m measureFixture) mergedPR(test *testing.T, number int, issueID string, state string, mergedAt *time.Time) {
	test.Helper()
	now := time.Now()
	cols := testutil.Cols{
		"workspace_id": m.fx.WorkspaceID, "installation_id": 1, "repo_owner": "o", "repo_name": "r",
		"pr_number": number, "title": "t", "state": state, "html_url": "https://example.test",
		"pr_created_at": now, "pr_updated_at": now, "head_sha": "a", "merge_commit_sha": "",
	}
	if mergedAt != nil {
		cols["merged_at"] = *mergedAt
	}
	pr := m.fx.Insert(test, "github_pull_request", cols)
	m.fx.InsertNoID(test, "issue_pull_request", testutil.Cols{
		"issue_id": issueID, "pull_request_id": pr, "link_kind": "closing",
	}, "pull_request_id = $1", pr)
}

func (m measureFixture) run(test *testing.T, agentID, issueID string, at time.Time) {
	test.Helper()
	m.fx.Task(test, agentID, testutil.Cols{"issue_id": issueID, "created_at": at, "status": "completed", "completed_at": at})
}

func TestRunBudgetMeasureM3RunsPerMergedPR(test *testing.T) {
	m := newMeasureFixture(test)
	runtime := m.fx.Runtime(test, "rt")
	agent := m.fx.Agent(test, "worker", runtime)
	at := func(text string) time.Time { return ictTime(test, text) }
	merged := func(text string) *time.Time { value := at(text); return &value }

	x := m.fx.Issue(test, "two PRs")
	y := m.fx.Issue(test, "one PR")
	z := m.fx.Issue(test, "open PR only")

	// Issue x: PR 1 merges on 31 Oct, PR 2 on 2 Nov. t1 and t2 are before PR 1
	// and count under it; t3 sits between the merges and counts under PR 2; t4
	// comes after every merge and counts nowhere.
	m.mergedPR(test, 1, x, "merged", merged("2026-10-31 12:00"))
	m.mergedPR(test, 2, x, "merged", merged("2026-11-02 12:00"))
	m.run(test, agent, x, at("2026-10-30 10:00"))
	m.run(test, agent, x, at("2026-10-31 09:00"))
	m.run(test, agent, x, at("2026-11-01 09:00"))
	m.run(test, agent, x, at("2026-11-03 09:00"))
	// Issue y: PR 3 merges late on 31 Oct ICT. The UTC day is still the 31st
	// for 17:00Z, so use 23:30 ICT, which is 16:30Z.
	m.mergedPR(test, 3, y, "merged", merged("2026-10-31 23:30"))
	m.run(test, agent, y, at("2026-10-31 08:00"))
	// Issue z: its PR is open, so its runs count nowhere.
	m.mergedPR(test, 4, z, "open", nil)
	m.run(test, agent, z, at("2026-10-31 08:00"))

	days := byDay(test, measureQuery(test, "M3", m.vars))
	if len(days) != 7 {
		test.Fatalf("M3 returned %d days, want 7", len(days))
	}
	for _, day := range []string{"2026-10-28", "2026-10-29", "2026-10-30"} {
		if days[day][0]["status"] != "unavailable" {
			test.Errorf("%s status = %v, want unavailable: before the first merged PR", day, days[day][0]["status"])
		}
		wantNull(test, day, days[day][0], "runs")
	}
	d31 := days["2026-10-31"][0]
	if d31["status"] != "ok" {
		test.Errorf("10-31 status = %v", d31["status"])
	}
	wantNumber(test, "10-31", d31, "merged_prs", 2)
	wantNumber(test, "10-31", d31, "runs", 3)
	if got, _ := number(d31, "runs_per_pr"); got != 1.5 {
		test.Errorf("10-31 runs_per_pr = %v, want 1.5", d31["runs_per_pr"])
	}
	d01 := days["2026-11-01"][0]
	if d01["status"] != "no PRs" {
		test.Errorf("11-01 status = %v, want \"no PRs\"", d01["status"])
	}
	wantNull(test, "11-01", d01, "runs_per_pr")
	d02 := days["2026-11-02"][0]
	wantNumber(test, "11-02", d02, "merged_prs", 1)
	wantNumber(test, "11-02", d02, "runs", 1)
	if days["2026-11-03"][0]["status"] != "no PRs" {
		test.Errorf("11-03 status = %v, want \"no PRs\"", days["2026-11-03"][0]["status"])
	}
}
