package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// No check reads the instruction, so the outcome must not depend on its language.
var askGuardWakeupTexts = map[string]string{
	"english":    "When the owner replies, decide whether to waive or rewrite it.",
	"vietnamese": "Khi chủ sở hữu trả lời, hãy quyết định bỏ qua hay viết lại.",
	"chinese":    "负责人回复后，决定是豁免还是重写。",
}

type askGuardWakeupFixture struct{ askGuardCommentFixture }

func newAskGuardWakeupFixture(t *testing.T) askGuardWakeupFixture {
	t.Helper()
	f := askGuardWakeupFixture{newAskGuardCommentFixture(t)}
	// The wakeup API needs the human who started the run.
	dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id = $1, accountable_user_id = $1, originator_source = 'trigger_owner' WHERE id = $2`, f.humanID, f.taskID)
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, f.issueID)
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE issue_id = $1`, f.issueID)
	dbfx.Cleanup(t, `DELETE FROM decision_request WHERE issue_id = $1`, f.issueID)
	return f
}

func (f askGuardWakeupFixture) wakeupReq(method, wakeupID string, body map[string]any) *http.Request {
	req := withURLParams(newRequest(method, "/api/issues/"+f.issueID+"/wakeups", body), "id", f.issueID, "wakeupID", wakeupID)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", f.agentID)
	req.Header.Set("X-Task-ID", f.taskID)
	return req
}

func (f askGuardWakeupFixture) create(t *testing.T, body map[string]any) *testutil.Response {
	t.Helper()
	if _, ok := body["agent_id"]; !ok {
		body["agent_id"] = f.agentID
	}
	return testutil.Call(t, testHandler.CreateIssueWakeup, f.wakeupReq(http.MethodPost, "", body))
}

func (f askGuardWakeupFixture) update(t *testing.T, id string, body map[string]any) *testutil.Response {
	t.Helper()
	body["agent_id"] = f.agentID
	return testutil.Call(t, testHandler.CreateIssueWakeup, f.wakeupReq(http.MethodPut, id, body))
}

// replyWait is the wait both incidents registered: a reply from the member.
func (f askGuardWakeupFixture) replyWait(text string, extra map[string]any) map[string]any {
	body := map[string]any{"kind": "event", "event_types": []string{"comment.created"}, "instruction": text}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (f askGuardWakeupFixture) memberWait(text string) map[string]any {
	return f.replyWait(text, map[string]any{"filter_actor_type": "member", "filter_actor_id": f.humanID})
}

func (f askGuardWakeupFixture) wakeups(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE issue_id = $1`, f.issueID)
}

// request seeds a decision request for the fixture's member.
func (f askGuardWakeupFixture) request(t *testing.T, issueID, humanID, status string) string {
	t.Helper()
	session := dbfx.ChatSession(t, f.agentID, testutil.Cols{"creator_id": humanID})
	id := dbfx.Insert(t, "decision_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": f.agentID, "human_user_id": humanID,
		"chat_session_id": session, "ask": "Waive or rewrite?", "escalation_class": "reversible",
		"origin": "asked", "status": status,
	})
	dbfx.Cleanup(t, `DELETE FROM decision_request WHERE id = $1`, id)
	return id
}

func (f askGuardWakeupFixture) wantFix(t *testing.T, res *testutil.Response) {
	t.Helper()
	for _, fix := range []string{"multica decision request", "--decision", "--purpose observe", "--filter-actor-type agent"} {
		if !strings.Contains(res.Text(), fix) {
			t.Errorf("422 body lacks %q: %s", fix, res.Text())
		}
	}
}

func TestAskGuardWakeupRefusesMemberEligibleRules(t *testing.T) {
	setAskGuardMode(t, "block")
	shapes := map[string]func(askGuardWakeupFixture, string) map[string]any{
		"member filter":    func(f askGuardWakeupFixture, text string) map[string]any { return f.memberWait(text) },
		"unfiltered reply": func(f askGuardWakeupFixture, text string) map[string]any { return f.replyWait(text, nil) },
		"issue change": func(f askGuardWakeupFixture, text string) map[string]any {
			return f.replyWait(text, map[string]any{"event_types": []string{"issue.status_changed"}})
		},
		"reaction": func(f askGuardWakeupFixture, text string) map[string]any {
			return f.replyWait(text, map[string]any{"event_types": []string{"reaction.added"}})
		},
	}
	for lang, text := range askGuardWakeupTexts {
		for name, shape := range shapes {
			t.Run(lang+"/"+name, func(t *testing.T) {
				f := newAskGuardWakeupFixture(t)
				res := f.create(t, shape(f, text)).Want(http.StatusUnprocessableEntity)
				f.wantFix(t, res)
				if n := f.wakeups(t); n != 0 {
					t.Errorf("refused wakeup saved %d rows", n)
				}
				if n := f.events(t, "block"); n != 1 {
					t.Errorf("block events = %d, want 1", n)
				}
			})
		}
	}
}

func TestAskGuardWakeupAllowedShapes(t *testing.T) {
	setAskGuardMode(t, "block")
	for lang, text := range askGuardWakeupTexts {
		t.Run(lang, func(t *testing.T) {
			f := newAskGuardWakeupFixture(t)
			cases := map[string]map[string]any{
				// A member comment cannot fire an agent-only rule.
				"agent-only observer": f.replyWait(text, map[string]any{"filter_actor_type": "agent"}),
				"agent with id":       f.replyWait(text, map[string]any{"filter_actor_type": "agent", "filter_actor_id": f.agentID}),
				// Declared observation never counts as a human wait.
				"observe": f.replyWait(text, map[string]any{"purpose": "observe"}),
				// Audit-only paths: nothing in the rule says a person is awaited.
				"timer":      {"kind": "at", "after_seconds": 600, "instruction": text},
				"task event": {"kind": "event", "event_types": []string{"task.completed"}, "instruction": text},
			}
			for name, body := range cases {
				f.create(t, body).Want(http.StatusCreated)
				if n := f.events(t, "block"); n != 0 {
					t.Fatalf("%s wrote %d block events", name, n)
				}
			}
			if n := f.wakeups(t); n != len(cases) {
				t.Errorf("wakeups = %d, want %d", n, len(cases))
			}
		})
	}
}

func TestAskGuardWakeupNotifyOnlyDoesNotSatisfyAWait(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardWakeupFixture(t)
	f.post(t, map[string]any{"content": "FYI " + f.mention(), "notify_only": true}).Want(http.StatusCreated)
	f.create(t, f.replyWait(askGuardWakeupTexts["english"], nil)).Want(http.StatusUnprocessableEntity)
}

func TestAskGuardWakeupBoundRequests(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardWakeupFixture(t)
	askA := f.request(t, f.issueID, f.humanID, "open")
	askB := f.request(t, f.issueID, f.humanID, "open")
	answered := f.request(t, f.issueID, f.humanID, "answered")
	text := askGuardWakeupTexts["vietnamese"]

	// Two asks in one run: each ask gets its own wait.
	f.create(t, f.replyWait(text, map[string]any{"decision_request_id": askA})).Want(http.StatusCreated)
	f.create(t, f.memberWait(text)).Want(http.StatusUnprocessableEntity) // an open request alone binds nothing
	f.create(t, f.replyWait(text, map[string]any{"decision_request_id": askB, "filter_actor_type": "member", "filter_actor_id": f.humanID})).Want(http.StatusCreated)
	// A request binds one enabled wakeup at a time.
	f.create(t, f.replyWait(text, map[string]any{"decision_request_id": askA})).Want(http.StatusConflict)
	// The answer arrived before the wakeup was registered.
	f.create(t, f.replyWait(text, map[string]any{"decision_request_id": answered})).Want(http.StatusCreated)
	if n := f.wakeups(t); n != 3 {
		t.Errorf("wakeups = %d, want 3", n)
	}
}

func TestAskGuardWakeupBindingMustMatch(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardWakeupFixture(t)
	otherIssue := dbfx.Issue(t, "other issue")
	otherMember := dbfx.User(t, "Other human", "other-"+f.humanID[:8]+"@test.invalid")
	dbfx.Member(t, testWorkspaceID, otherMember, "member")
	text := askGuardWakeupTexts["chinese"]
	cases := map[string]string{
		"request on another issue":  f.request(t, otherIssue, f.humanID, "open"),
		"request for another human": f.request(t, f.issueID, otherMember, "open"),
		"cancelled request":         f.request(t, f.issueID, f.humanID, "cancelled"),
		"unknown request":           "00000000-0000-4000-8000-000000000000",
		"not a uuid":                "not-a-request",
	}
	for name, id := range cases {
		extra := map[string]any{"decision_request_id": id, "filter_actor_type": "member", "filter_actor_id": f.humanID}
		if res := f.create(t, f.replyWait(text, extra)); res.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, res.Code, res.Text())
		}
	}
	if n := f.events(t, "block"); n != 0 {
		t.Errorf("a malformed binding is a validation error, not a guard event: %d block events", n)
	}
}

func TestAskGuardWakeupPurposeIsValidated(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardWakeupFixture(t)
	ask := f.request(t, f.issueID, f.humanID, "open")
	f.create(t, f.replyWait("x", map[string]any{"purpose": "poll"})).Want(http.StatusBadRequest)
	// A bound wait is a human wait; observe is never one.
	f.create(t, f.replyWait("x", map[string]any{"purpose": "observe", "decision_request_id": ask})).Want(http.StatusBadRequest)
}

func TestAskGuardWakeupChecksTheEffectiveRuleOnUpdate(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardWakeupFixture(t)
	text := askGuardWakeupTexts["english"]
	var created struct {
		ID string `json:"id"`
	}
	f.create(t, f.replyWait(text, map[string]any{"filter_actor_type": "agent"})).Want(http.StatusCreated).JSON(&created)
	// Dropping the agent-only filter makes the rule member-eligible.
	f.update(t, created.ID, f.replyWait(text, nil)).Want(http.StatusUnprocessableEntity)
	if n := f.events(t, "block"); n != 1 {
		t.Errorf("block events = %d, want 1", n)
	}
	// The stored rule did not change.
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE id = $1 AND filter_actor_type = 'agent'`, created.ID); n != 1 {
		t.Errorf("refused update changed the stored rule")
	}
	ask := f.request(t, f.issueID, f.humanID, "open")
	f.update(t, created.ID, f.replyWait(text, map[string]any{"decision_request_id": ask})).Want(http.StatusOK)
}

func TestAskGuardWakeupModes(t *testing.T) {
	text := askGuardWakeupTexts["english"]
	t.Run("warn stores an event and allows", func(t *testing.T) {
		setAskGuardMode(t, "warn")
		f := newAskGuardWakeupFixture(t)
		f.create(t, f.memberWait(text)).Want(http.StatusCreated)
		if n := f.events(t, "warn"); n != 1 {
			t.Errorf("warn events = %d, want 1", n)
		}
	})
	t.Run("off checks nothing", func(t *testing.T) {
		f := newAskGuardWakeupFixture(t)
		f.create(t, f.memberWait(text)).Want(http.StatusCreated)
		if n := f.events(t, "warn") + f.events(t, "block"); n != 0 {
			t.Errorf("events = %d, want 0", n)
		}
	})
}

func TestAskGuardWakeupChecksTheStoredRuleOnEnable(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardWakeupFixture(t)
	rule := uuid.NewString()
	dbfx.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,created_by,instruction,kind,mode,event_types,enabled,disabled_at)
		VALUES($1,$2,$3,$4,$5,$6,'event','once','{comment.created}',false,now())`, rule, testWorkspaceID, f.issueID, f.agentID, f.humanID, askGuardWakeupTexts["english"])
	enable := func() *testutil.Response {
		req := withURLParams(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/wakeups/"+rule+"/enable", map[string]any{"revision": 1}), "id", f.issueID, "wakeupID", rule)
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Agent-ID", f.agentID)
		req.Header.Set("X-Task-ID", f.taskID)
		return testutil.Call(t, testHandler.EnableIssueWakeup, req)
	}
	f.wantFix(t, enable().Want(http.StatusUnprocessableEntity))
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE id = $1 AND enabled`, rule); n != 0 {
		t.Error("refused enable switched the rule on")
	}
	// A rule bound to a request enables.
	dbfx.Exec(t, `UPDATE issue_wakeup SET decision_request_id = $2 WHERE id = $1`, rule, f.request(t, f.issueID, f.humanID, "open"))
	enable().Want(http.StatusOK)
}

// Two refused waits of one shape are two asks: each keeps its own event so
// recovery can serve both. A retry of the same request is still one event.
func TestAskGuardWakeupDistinctWaitsKeepDistinctEvents(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardWakeupFixture(t)
	first := f.memberWait("Ask the owner about the quote.")
	second := f.memberWait("Ask the owner about the schedule.")
	f.wantFix(t, f.create(t, first).Want(http.StatusUnprocessableEntity))
	f.wantFix(t, f.create(t, second).Want(http.StatusUnprocessableEntity))
	f.wantFix(t, f.create(t, first).Want(http.StatusUnprocessableEntity))
	if n := f.events(t, "block"); n != 2 {
		t.Errorf("block events = %d, want 2 (one per distinct wait, retry deduplicated)", n)
	}
}
