package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// holdFixture is a project with an active intake_hold entry: the window opened
// an hour ago and closes in an hour, mode block.
type holdFixture struct {
	projectID string
	entry     map[string]any
}

func newHoldFixture(test *testing.T, edit func(entry map[string]any)) holdFixture {
	test.Helper()
	projectID := dbfx.Project(test, "intake hold")
	now := time.Now().UTC()
	entry := map[string]any{
		"project_id": projectID, "mode": "block", "source": "CHE-6911",
		"starts_at": now.Add(-time.Hour).Format(time.RFC3339), "ends_at": now.Add(time.Hour).Format(time.RFC3339),
	}
	if edit != nil {
		edit(entry)
	}
	body, err := json.Marshal([]any{entry})
	if err != nil {
		test.Fatal(err)
	}
	setWorkspaceSettings(test, fmt.Sprintf(`{"intake_hold":%s}`, body))
	return holdFixture{projectID, entry}
}

func (fixture holdFixture) issue(test *testing.T, title string, over ...testutil.Cols) string {
	test.Helper()
	cols := testutil.Cols{"status": "backlog", "project_id": fixture.projectID}
	for _, extra := range over {
		for key, value := range extra {
			cols[key] = value
		}
	}
	return dbfx.Issue(test, title, cols)
}

// readHold returns the intake_hold field of GET /api/issues/{id}, nil when the
// response carries none.
func readHold(test *testing.T, issueID string) map[string]any {
	test.Helper()
	request := withURLParam(newRequest("GET", "/api/issues/"+issueID, nil), "id", issueID)
	recorder := httptest.NewRecorder()
	testHandler.GetIssue(recorder, request)
	if recorder.Code != http.StatusOK {
		test.Fatalf("GET issue: %d %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		test.Fatal(err)
	}
	raw, present := body["intake_hold"]
	if !present {
		return nil
	}
	var field map[string]any
	if err := json.Unmarshal(raw, &field); err != nil {
		test.Fatal(err)
	}
	return field
}

func wantHold(test *testing.T, issueID string, held, owned bool, reason string) map[string]any {
	test.Helper()
	field := readHold(test, issueID)
	if field == nil {
		test.Fatalf("intake_hold absent, want held=%v owned=%v reason=%s", held, owned, reason)
	}
	if field["held"] != held || field["owned"] != owned || field["reason"] != reason {
		test.Fatalf("intake_hold = %v, want held=%v owned=%v reason=%s", field, held, owned, reason)
	}
	return field
}

func statusRow(test *testing.T, issueID, actorType, kind, source, from, to string, extra map[string]any) {
	test.Helper()
	details := map[string]any{"from": from, "to": to, "trigger": map[string]any{"v": 1, "cause": map[string]any{"kind": kind}, "auth_source": source}}
	for key, value := range extra {
		details[key] = value
	}
	body, err := json.Marshal(details)
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Insert(test, "activity_log", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "actor_type": actorType, "action": "status_changed", "details": string(body)})
}

func TestIntakeHoldReadFieldHeld(test *testing.T) {
	fixture := newHoldFixture(test, nil)
	issueID := fixture.issue(test, "feat: new request")
	field := wantHold(test, issueID, true, true, "held")
	if field["source"] != "CHE-6911" || field["ends_at"] != fixture.entry["ends_at"] {
		test.Fatalf("source/ends_at = %v / %v", field["source"], field["ends_at"])
	}
	if _, present := field["exempt_after"]; present {
		test.Fatalf("exempt_after on an issue with no pending answer: %v", field)
	}
}

func TestIntakeHoldReadFieldAbsent(test *testing.T) {
	fixture := newHoldFixture(test, nil)
	parent := fixture.issue(test, "feat: parent")
	if field := readHold(test, fixture.issue(test, "feat: child", testutil.Cols{"parent_issue_id": parent})); field != nil {
		test.Fatalf("an issue with a parent carries %v", field)
	}
	other := dbfx.Project(test, "no entry")
	if field := readHold(test, dbfx.Issue(test, "feat: elsewhere", testutil.Cols{"status": "backlog", "project_id": other})); field != nil {
		test.Fatalf("a project with no entry carries %v", field)
	}
	if field := readHold(test, dbfx.Issue(test, "feat: no project", testutil.Cols{"status": "backlog"})); field != nil {
		test.Fatalf("an issue with no project carries %v", field)
	}
}

func TestIntakeHoldReadFieldRules(test *testing.T) {
	fixture := newHoldFixture(test, func(entry map[string]any) {
		entry["exempt_types"] = []string{"critic"}
		entry["exempt_labels"] = []string{"adoption"}
		entry["exempt_escalation_classes"] = []string{"spend"}
	})
	test.Run("no title type is held", func(test *testing.T) {
		wantHold(test, fixture.issue(test, "plain words"), true, true, "held")
	})
	test.Run("priority exempts nothing", func(test *testing.T) {
		wantHold(test, fixture.issue(test, "feat: urgent", testutil.Cols{"priority": "urgent"}), true, true, "held")
	})
	test.Run("outside backlog", func(test *testing.T) {
		wantHold(test, fixture.issue(test, "feat: moved", testutil.Cols{"status": "in_progress"}), false, false, "left_backlog")
	})
	test.Run("created before the window", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: old", testutil.Cols{"created_at": testutil.Raw("now() - interval '2 hours'")})
		wantHold(test, issueID, false, false, "created_before_start")
	})
	test.Run("exempt type", func(test *testing.T) {
		wantHold(test, fixture.issue(test, "critic: review it"), false, false, "exempt_type")
	})
	test.Run("exempt label", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: labelled")
		labelID := dbfx.Insert(test, "issue_label", testutil.Cols{"workspace_id": testWorkspaceID, "name": "adoption", "color": "#112233"})
		dbfx.InsertNoID(test, "issue_to_label", testutil.Cols{"issue_id": issueID, "label_id": labelID}, "issue_id = $1", issueID)
		wantHold(test, issueID, false, false, "exempt_label")
	})
	test.Run("exempt issue id", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: named")
		fixture := newHoldFixtureWith(test, fixture, func(entry map[string]any) { entry["exempt_issue_ids"] = []string{issueID} })
		wantHold(test, issueID, false, false, "exempt_issue_id")
		_ = fixture
	})
}

// newHoldFixtureWith rewrites the entry of an existing fixture.
func newHoldFixtureWith(test *testing.T, fixture holdFixture, edit func(entry map[string]any)) holdFixture {
	test.Helper()
	edit(fixture.entry)
	body, err := json.Marshal([]any{fixture.entry})
	if err != nil {
		test.Fatal(err)
	}
	setWorkspaceSettings(test, fmt.Sprintf(`{"intake_hold":%s}`, body))
	return fixture
}

func TestIntakeHoldReadFieldWindow(test *testing.T) {
	now := time.Now().UTC()
	test.Run("after ends_at the issue is owned and not held", func(test *testing.T) {
		fixture := newHoldFixture(test, func(entry map[string]any) { entry["ends_at"] = now.Add(-time.Minute).Format(time.RFC3339) })
		wantHold(test, fixture.issue(test, "feat: late"), false, true, "ended")
	})
	test.Run("mode off", func(test *testing.T) {
		fixture := newHoldFixture(test, func(entry map[string]any) { entry["mode"] = "off" })
		wantHold(test, fixture.issue(test, "feat: off"), false, true, "mode_off")
	})
	test.Run("warn holds like block", func(test *testing.T) {
		fixture := newHoldFixture(test, func(entry map[string]any) { entry["mode"] = "warn" })
		wantHold(test, fixture.issue(test, "feat: warn"), true, true, "held")
	})
}

func TestIntakeHoldReadFieldPersonRelease(test *testing.T) {
	fixture := newHoldFixture(test, nil)
	// A release leaves backlog; the rule must still see it when the issue is back.
	test.Run("form a: a person moved it out of backlog", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: a")
		statusRow(test, issueID, "member", "ui_or_api", "interactive_jwt", "backlog", "todo", nil)
		wantHold(test, issueID, false, false, "released_by_member")
	})
	test.Run("form b: the server's release key", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: b")
		statusRow(test, issueID, "member", "ui_or_api", "pat", "need_decision", "todo", map[string]any{"intake_release": "CHE-6911"})
		wantHold(test, issueID, false, false, "released_by_member")
	})
	test.Run("a machine credential never releases", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: machine")
		statusRow(test, issueID, "member", "ui_or_api", "cloud_pat", "backlog", "todo", nil)
		statusRow(test, issueID, "agent", "ui_or_api", "task_token", "backlog", "todo", nil)
		wantHold(test, issueID, true, true, "held")
	})
	test.Run("a move back to backlog is no release", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: back")
		statusRow(test, issueID, "member", "ui_or_api", "interactive_jwt", "todo", "backlog", nil)
		wantHold(test, issueID, true, true, "held")
	})
}

// escalationAnswer files a human request of the class with a person's answer
// whose wake_at is the given SQL expression.
func escalationAnswer(test *testing.T, issueID, class, wakeAt string, over ...testutil.Cols) {
	test.Helper()
	runtimeID := dbfx.Runtime(test, "hold rt "+issueID[:8])
	agentID := dbfx.Agent(test, "hold agent "+issueID[:8], runtimeID)
	sessionID := dbfx.ChatSession(test, agentID)
	requestID := dbfx.Insert(test, "decision_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": testUserID,
		"chat_session_id": sessionID, "escalation_class": class, "status": "answered", "origin": "asked", "authority": "human",
	})
	answer := testutil.Cols{
		"decision_request_id": requestID, "answer_source": "card", "answered_by_user_id": testUserID,
		"chat_message_ids": testutil.Raw("'{}'::uuid[]"), "content_snapshot": "yes",
		"effective_at": testutil.Raw("now()"), "wake_at": testutil.Raw(wakeAt),
	}
	for _, extra := range over {
		for key, value := range extra {
			answer[key] = value
		}
	}
	dbfx.Insert(test, "decision_answer", answer)
}

func TestIntakeHoldReadFieldEscalationAnswer(test *testing.T) {
	fixture := newHoldFixture(test, func(entry map[string]any) { entry["exempt_escalation_classes"] = []string{"spend"} })
	test.Run("an answer in effect exempts the issue", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: paid")
		escalationAnswer(test, issueID, "spend", "now() - interval '1 second'")
		wantHold(test, issueID, false, false, "exempt_escalation_class")
	})
	test.Run("an answer not yet effective keeps the hold and names exempt_after", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: waiting")
		escalationAnswer(test, issueID, "spend", "now() + interval '10 minutes'")
		field := wantHold(test, issueID, true, true, "held")
		var wake time.Time
		dbfx.QueryRow(test, `SELECT wake_at FROM decision_answer WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, issueID).Scan(&wake)
		after, _ := field["exempt_after"].(string)
		parsed, err := time.Parse(time.RFC3339Nano, after)
		if err != nil || !parsed.Equal(wake) {
			test.Fatalf("exempt_after = %q (%v), want %s", after, err, wake.Format(time.RFC3339Nano))
		}
		dbfx.Exec(test, `UPDATE decision_answer SET wake_at = now() - interval '1 second' WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, issueID)
		wantHold(test, issueID, false, false, "exempt_escalation_class")
	})
	test.Run("a withdrawn answer exempts nothing", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: withdrawn")
		escalationAnswer(test, issueID, "spend", "now() - interval '1 second'", testutil.Cols{"disclaimed_at": testutil.Raw("now()")})
		wantHold(test, issueID, true, true, "held")
	})
	test.Run("a class the entry does not name exempts nothing", func(test *testing.T) {
		issueID := fixture.issue(test, "feat: other class")
		escalationAnswer(test, issueID, "deploy", "now() - interval '1 second'")
		wantHold(test, issueID, true, true, "held")
	})
}

func TestIntakeHoldReadFieldAutopilot(test *testing.T) {
	runtimeID := dbfx.Runtime(test, "hold autopilot rt")
	agentID := dbfx.Agent(test, "hold autopilot agent", runtimeID)
	autopilotID := dbfx.Insert(test, "autopilot", testutil.Cols{"workspace_id": testWorkspaceID, "title": "adoption", "assignee_id": agentID, "created_by_type": "member", "created_by_id": testUserID})
	runID := dbfx.Insert(test, "autopilot_run", testutil.Cols{"autopilot_id": autopilotID, "source": "manual", "status": "issue_created"})
	taskID := dbfx.Task(test, agentID, testutil.Cols{"autopilot_run_id": runID, "runtime_id": runtimeID})
	fixture := newHoldFixture(test, func(entry map[string]any) { entry["exempt_autopilot_ids"] = []string{autopilotID} })
	issueID := fixture.issue(test, "feat: made by the autopilot", testutil.Cols{"origin_type": "agent_create", "origin_id": taskID})
	wantHold(test, issueID, false, false, "exempt_autopilot")
	if field := readHold(test, fixture.issue(test, "feat: not by it")); field["held"] != true {
		test.Fatalf("an issue the autopilot did not create is %v", field)
	}
}

func TestIntakeHoldMalformedStoredValueHoldsNothing(test *testing.T) {
	projectID := dbfx.Project(test, "malformed")
	setWorkspaceSettings(test, `{"intake_hold":{"not":"an array"}}`)
	issueID := dbfx.Issue(test, "feat: x", testutil.Cols{"status": "backlog", "project_id": projectID})
	if field := readHold(test, issueID); field != nil {
		test.Fatalf("a malformed stored value produced %v", field)
	}
}
