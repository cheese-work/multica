package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// repairEvent ends the fixture's run and records one block event for it through
// the real insert, then lets the recovery notice route it to repair.
func repairEvent(t *testing.T, fixture intentFixture, kind string, member, source any) string {
	t.Helper()
	setAskGuardMode(t, "block")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	eventID := insertRepairEvent(t, fixture, fixture.taskID, kind, member, source)
	deliverRecoveries(t)
	return eventID
}

func insertRepairEvent(t *testing.T, fixture intentFixture, taskID, kind string, member, source any) string {
	t.Helper()
	params := db.InsertAskGuardEventParams{ID: parseUUID(uuid.NewString()), WorkspaceID: parseUUID(testWorkspaceID), IssueID: parseUUID(fixture.issueID),
		TaskID: parseUUID(taskID), OperationKind: kind, PayloadSha256: uuid.NewString(), Mode: "block"}
	if m, ok := member.(string); ok {
		params.MemberID = parseUUID(m)
	}
	if s, ok := source.(string); ok {
		params.SourceText = pgtype.Text{String: s, Valid: true}
	}
	event, err := testHandler.Queries.InsertAskGuardEvent(context.Background(), params)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM ask_repair_root WHERE issue_id = $1`, fixture.issueID)
		testPool.Exec(context.Background(), `DELETE FROM ask_guard_event WHERE issue_id = $1`, fixture.issueID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1 AND author_type = 'system'`, fixture.issueID)
	})
	return uuidToString(event.ID)
}

func admitRepairs(t *testing.T) {
	t.Helper()
	if err := testHandler.admitAskRepairs(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func settleRepairs(t *testing.T) {
	t.Helper()
	if err := testHandler.settleAskRepairs(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func repairTasks(t *testing.T, fixture intentFixture) (ids []string, notes []string) {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT id::text, COALESCE(handoff_note, '') FROM agent_task_queue WHERE issue_id = $1 AND id <> $2 ORDER BY created_at`, fixture.issueID, fixture.taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, note string
		if err := rows.Scan(&id, &note); err != nil {
			t.Fatal(err)
		}
		ids, notes = append(ids, id), append(notes, note)
	}
	return ids, notes
}

func rootState(t *testing.T, eventID string) (state, reason string) {
	t.Helper()
	var r *string
	dbfx.QueryRow(t, `SELECT state, failure_reason FROM ask_repair_root WHERE id = $1`, eventID).Scan(&state, &r)
	if r != nil {
		reason = *r
	}
	return state, reason
}

func systemComments(t *testing.T, fixture intentFixture) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' ORDER BY created_at`, fixture.issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatal(err)
		}
		out = append(out, content)
	}
	return out
}

// TestAskRepairPureS1RunsAndSendsNoNotice: a source-less wakeup event goes to a
// repair run for the agent that produced it. The run message holds the exact
// request command; nobody gets a notice. A repeated pass admits nothing more.
func TestAskRepairPureS1RunsAndSendsNoNotice(t *testing.T) {
	fixture := newIntentFixture(t)
	eventID := repairEvent(t, fixture, "wakeup", nil, nil)
	admitRepairs(t)
	admitRepairs(t)

	ids, notes := repairTasks(t, fixture)
	if len(ids) != 1 {
		t.Fatalf("repair tasks = %d, want 1", len(ids))
	}
	for _, want := range []string{"multica decision request", "--issue " + fixture.issueID, "--recovers " + eventID} {
		if !strings.Contains(notes[0], want) {
			t.Fatalf("run message lacks %q:\n%s", want, notes[0])
		}
	}
	var agentID string
	dbfx.QueryRow(t, `SELECT agent_id::text FROM agent_task_queue WHERE id = $1`, ids[0]).Scan(&agentID)
	if agentID != fixture.agentID {
		t.Fatalf("repair went to %s, want the producing agent %s", agentID, fixture.agentID)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ask_recovery_notice WHERE event_id = $1`, eventID) + noticeMessagesFor(t, fixture); n != 0 {
		t.Fatalf("a notice was sent: %d rows", n)
	}
	var taskID string
	dbfx.QueryRow(t, `SELECT repair_task_id::text FROM ask_repair_root WHERE id = $1`, eventID).Scan(&taskID)
	if taskID != ids[0] {
		t.Fatalf("root admits task %s, want %s", taskID, ids[0])
	}
}

func onlyRepairTask(t *testing.T, fixture intentFixture) string {
	t.Helper()
	ids, _ := repairTasks(t, fixture)
	if len(ids) != 1 {
		t.Fatalf("repair tasks = %d, want 1", len(ids))
	}
	return ids[0]
}

func noticeMessagesFor(t *testing.T, fixture intentFixture) int {
	return dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%automatic recovery%'`, fixture.issueID)
}

// TestAskRepairFreshEventGetsNoSecondRepair: an event the repair run produces
// inherits the root, so it never admits a second repair. The root fails, owned
// by the agent, with one system comment that mentions nobody.
func TestAskRepairFreshEventGetsNoSecondRepair(t *testing.T) {
	fixture := newIntentFixture(t)
	eventID := repairEvent(t, fixture, "wakeup", nil, nil)
	admitRepairs(t)
	repairTask := onlyRepairTask(t, fixture)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, repairTask)

	fresh := insertRepairEvent(t, fixture, repairTask, "wakeup", nil, nil)
	var root string
	dbfx.QueryRow(t, `SELECT root_id::text FROM ask_guard_event WHERE id = $1`, fresh).Scan(&root)
	if root != eventID {
		t.Fatalf("fresh event root = %s, want %s", root, eventID)
	}
	deliverRecoveries(t)
	admitRepairs(t)
	admitRepairs(t)
	settleRepairs(t)

	if again, _ := repairTasks(t, fixture); len(again) != 1 {
		t.Fatalf("repair tasks = %d, want still 1", len(again))
	}
	if state, reason := rootState(t, eventID); state != "failed" || reason != "repair_created_ask" {
		t.Fatalf("root = %s/%s, want failed/repair_created_ask", state, reason)
	}
	comments := systemComments(t, fixture)
	if len(comments) != 1 {
		t.Fatalf("system comments = %d, want 1", len(comments))
	}
	if !strings.Contains(comments[0], fixture.agentName) || util.MentionRe.MatchString(comments[0]) {
		t.Fatalf("comment must name the agent and mention nobody:\n%s", comments[0])
	}
}

// TestAskRepairRestartDuringAdmission: a crash before the commit leaves no
// root and no task; the retry admits exactly one.
func TestAskRepairRestartDuringAdmission(t *testing.T) {
	fixture := newIntentFixture(t)
	eventID := repairEvent(t, fixture, "wakeup", nil, nil)
	askRepairFault = func(point string) error {
		if point == "before_commit" {
			return errors.New("crash at " + point)
		}
		return nil
	}
	t.Cleanup(func() { askRepairFault = nil })
	if err := testHandler.admitAskRepairs(context.Background()); err == nil {
		t.Fatal("expected the injected crash")
	}
	askRepairFault = nil
	if ids, _ := repairTasks(t, fixture); len(ids) != 0 || dbfx.Count(t, `SELECT count(*) FROM ask_repair_root WHERE id = $1`, eventID) != 0 {
		t.Fatal("a crashed admission left a task or a root")
	}
	admitRepairs(t)
	admitRepairs(t)
	if ids, _ := repairTasks(t, fixture); len(ids) != 1 {
		t.Fatalf("repair tasks after retry = %d, want 1", len(ids))
	}
}

// TestAskRepairUnavailableAgentFails: no agent to repair means an owned
// failure, one comment, no task.
func TestAskRepairUnavailableAgentFails(t *testing.T) {
	fixture := newIntentFixture(t)
	eventID := repairEvent(t, fixture, "wakeup", nil, nil)
	dbfx.Exec(t, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
	admitRepairs(t)
	admitRepairs(t)
	if ids, _ := repairTasks(t, fixture); len(ids) != 0 {
		t.Fatalf("repair tasks = %d, want 0", len(ids))
	}
	if state, reason := rootState(t, eventID); state != "failed" || reason != "agent_unavailable" {
		t.Fatalf("root = %s/%s, want failed/agent_unavailable", state, reason)
	}
	if got := systemComments(t, fixture); len(got) != 1 {
		t.Fatalf("system comments = %d, want 1", len(got))
	}
}

// TestAskRepairNoAccessRecipientGetsNoExcerpt: recipient_no_access still goes
// to repair, and neither the run message nor anything else repeats the source.
func TestAskRepairNoAccessRecipientGetsNoExcerpt(t *testing.T) {
	fixture := newIntentFixture(t)
	outsider := dbfx.User(t, "Outsider", "outsider-"+uuid.NewString()+"@test.invalid")
	eventID := repairEvent(t, fixture, "comment", outsider, "classified wording 7731")
	var reason string
	dbfx.QueryRow(t, `SELECT repair_reason FROM ask_guard_event WHERE id = $1`, eventID).Scan(&reason)
	if reason != askRecoveryNoAccess {
		t.Fatalf("repair_reason = %s, want %s", reason, askRecoveryNoAccess)
	}
	admitRepairs(t)
	ids, notes := repairTasks(t, fixture)
	if len(ids) != 1 || strings.Contains(notes[0], "classified wording 7731") || !strings.Contains(notes[0], "--recovers "+eventID) {
		t.Fatalf("want one repair run with the command and no excerpt, got %d: %v", len(ids), notes)
	}
}

// TestAskRepairSettlesOrFailsWhenRunEnds: a repair run that cleared the event
// settles the root; one that ended without a matching request fails it.
func TestAskRepairSettlesOrFailsWhenRunEnds(t *testing.T) {
	for _, c := range []struct {
		name, state, reason string
		clear               bool
	}{
		{"request made", "settled", "", true},
		{"no request", "failed", "no_matching_request", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fixture := newIntentFixture(t)
			eventID := repairEvent(t, fixture, "wakeup", nil, nil)
			admitRepairs(t)
			repairTask := onlyRepairTask(t, fixture)
			settleRepairs(t)
			if state, _ := rootState(t, eventID); state != "admitted" {
				t.Fatalf("root = %s while the repair run is live, want admitted", state)
			}
			if c.clear {
				dbfx.Exec(t, `UPDATE ask_guard_event SET cleared_at = clock_timestamp() WHERE id = $1`, eventID)
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, repairTask)
			settleRepairs(t)
			settleRepairs(t)
			if state, reason := rootState(t, eventID); state != c.state || reason != c.reason {
				t.Fatalf("root = %s/%s, want %s/%s", state, reason, c.state, c.reason)
			}
			want := 0
			if !c.clear {
				want = 1
			}
			if got := systemComments(t, fixture); len(got) != want {
				t.Fatalf("system comments = %d, want %d", len(got), want)
			}
		})
	}
}

// TestAskRepairSkipsChatLaneRuns: a chat-lane run only warns, so a block event
// naming one is never repaired.
func TestAskRepairSkipsChatLaneRuns(t *testing.T) {
	fixture := newIntentFixture(t)
	session := dbfx.ChatSession(t, fixture.agentID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_session_id = $2 WHERE id = $1`, fixture.taskID, session)
	eventID := repairEvent(t, fixture, "wakeup", nil, nil)
	admitRepairs(t)
	if ids, _ := repairTasks(t, fixture); len(ids) != 0 || dbfx.Count(t, `SELECT count(*) FROM ask_repair_root WHERE id = $1`, eventID) != 0 {
		t.Fatal("a chat-lane event was repaired")
	}
}

// TestAskRepairFailureListedInBrief: an owned failure shows in the decision
// brief with its agent and a source excerpt for a workspace member.
func TestAskRepairFailureListedInBrief(t *testing.T) {
	fixture := newIntentFixture(t)
	eventID := repairEvent(t, fixture, "comment", nil, "please pick the plan")
	dbfx.Exec(t, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
	admitRepairs(t)

	w := httptest.NewRecorder()
	testHandler.GetAgentDecisionBrief(w, newRequest(http.MethodGet, "/api/agent-decisions/brief", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("brief = %d %s", w.Code, w.Body.String())
	}
	var out struct {
		AskFailures []struct {
			RootID  string `json:"root_id"`
			IssueID string `json:"issue_id"`
			Agent   string `json:"agent"`
			Reason  string `json:"reason"`
			Excerpt string `json:"excerpt"`
		} `json:"ask_failures"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, f := range out.AskFailures {
		if f.RootID == eventID {
			if f.IssueID != fixture.issueID || f.Agent != fixture.agentName || f.Reason != "agent_unavailable" || f.Excerpt != "please pick the plan" {
				t.Fatalf("failure = %+v", f)
			}
			return
		}
	}
	t.Fatalf("root %s missing from ask_failures: %s", eventID, w.Body.String())
}

// TestAskRepairMemberlessRequestSettlesRoot: a source-less wakeup event has no
// member, yet the request the repair run makes clears it and settles the root.
// An event that was never routed to repair stays unrecoverable.
func TestAskRepairMemberlessRequestSettlesRoot(t *testing.T) {
	fixture := newSingleCallFixture(t)
	stray := uuid.NewString()
	dbfx.Exec(t, `INSERT INTO ask_guard_event (id, workspace_id, issue_id, task_id, operation_kind, payload_sha256, mode) VALUES ($1, $2, $3, $4, 'wakeup', $5, 'block')`,
		stray, testWorkspaceID, fixture.issueID, uuid.NewString(), uuid.NewString())
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM ask_guard_event WHERE id = $1`, stray) })
	eventID := repairEvent(t, fixture.intentFixture, "wakeup", nil, nil)
	admitRepairs(t)
	repairTask := onlyRepairTask(t, fixture.intentFixture)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, repairTask)

	body := fixture.body()
	body["recovers"] = stray
	request := fixture.call(body)
	request.Header.Set("X-Task-ID", repairTask)
	testutil.Call(t, testHandler.RequestDecision, request).Want(http.StatusBadRequest)

	body["recovers"] = eventID
	request = fixture.call(body)
	request.Header.Set("X-Task-ID", repairTask)
	testutil.Call(t, testHandler.RequestDecision, request).Want(http.StatusCreated)
	if !eventCleared(t, eventID) || eventCleared(t, stray) {
		t.Fatal("the repair request must clear its own event and nothing else")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, repairTask)
	settleRepairs(t)
	settleRepairs(t)
	if state, reason := rootState(t, eventID); state != "settled" || reason != "" {
		t.Fatalf("root = %s/%s, want settled", state, reason)
	}
}

// TestAskRepairMessageNamesClass: the CLI refuses a request without --class, so
// the command in the run message must carry it.
func TestAskRepairMessageNamesClass(t *testing.T) {
	message := askRepairMessage(db.AskGuardEvent{ID: parseUUID(uuid.NewString())}, db.Issue{ID: parseUUID(uuid.NewString())})
	if !strings.Contains(message, "--class ") {
		t.Fatalf("run message lacks --class:\n%s", message)
	}
}

// TestAskRepairDoesNotUseReviewException: the producing agent is the reviewer of
// an in_review issue whose budget is spent and whose head is unreviewed. A repair
// is not a review, so the budget refuses it and no head admission is consumed.
func TestAskRepairDoesNotUseReviewException(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 3363001, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_review")
	linkPR(t, c.issueID, 3363, "head-repair")
	_, err := testHandler.TaskService.EnqueueAskRepairTaskInTx(context.Background(), testHandler.Queries, issue, issue.AssigneeID, "repair")
	if !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("repair over the cap = %v, want ErrRunBudgetExhausted", err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_review_admission WHERE issue_id = $1`, c.issueID); n != 0 {
		t.Fatalf("repair consumed %d head admissions", n)
	}
}

// TestAskRepairFailureWithoutAgentRowStillListed: a failed root whose agent is
// gone stays in the brief with the agent id as its owner.
func TestAskRepairFailureWithoutAgentRowStillListed(t *testing.T) {
	fixture := newIntentFixture(t)
	eventID := repairEvent(t, fixture, "wakeup", nil, nil)
	dbfx.Exec(t, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
	admitRepairs(t)
	dbfx.Exec(t, `DELETE FROM agent WHERE id = $1`, fixture.agentID)
	rows, err := testHandler.Queries.ListFailedAskRepairRoots(context.Background(), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if uuidToString(row.ID) == eventID {
			if row.AgentName != fixture.agentID {
				t.Fatalf("owner = %q, want the agent id %s", row.AgentName, fixture.agentID)
			}
			return
		}
	}
	t.Fatalf("root %s missing with its agent row gone", eventID)
}

// bystanderRequest sends a decision request recovering eventID from a running
// task that is not the admitted repair run.
func bystanderRequest(t *testing.T, fixture singleCallFixture, eventID string) *testutil.Response {
	t.Helper()
	bystander := dbfx.Task(t, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(t)})
	body := fixture.body()
	body["recovers"] = eventID
	request := fixture.call(body)
	request.Header.Set("X-Task-ID", bystander)
	return testutil.Call(t, testHandler.RequestDecision, request)
}

// TestAskRepairBystanderCannotMaskFailure: only the admitted repair run recovers
// a memberless event. Another task's request is refused, so a repair that ends
// with no request of its own still fails the root.
func TestAskRepairBystanderCannotMaskFailure(t *testing.T) {
	fixture := newSingleCallFixture(t)
	eventID := repairEvent(t, fixture.intentFixture, "wakeup", nil, nil)
	admitRepairs(t)
	repairTask := onlyRepairTask(t, fixture.intentFixture)

	bystanderRequest(t, fixture, eventID).Want(http.StatusBadRequest)
	if eventCleared(t, eventID) {
		t.Fatal("a bystander task cleared the event")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, repairTask)
	settleRepairs(t)
	settleRepairs(t)
	if state, reason := rootState(t, eventID); state != "failed" || reason != "no_matching_request" {
		t.Fatalf("root = %s/%s, want failed/no_matching_request", state, reason)
	}
}

// TestAskRepairFailedRootCannotBeRecovered: a root that failed (refused admission,
// unavailable agent) has no live repair run, so nothing recovers its event.
func TestAskRepairFailedRootCannotBeRecovered(t *testing.T) {
	fixture := newSingleCallFixture(t)
	eventID := repairEvent(t, fixture.intentFixture, "wakeup", nil, nil)
	admitRepairs(t)
	repairTask := onlyRepairTask(t, fixture.intentFixture)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, repairTask)
	dbfx.Exec(t, `UPDATE ask_repair_root SET state = 'failed', failure_reason = 'admission_refused', failed_at = clock_timestamp() WHERE id = $1`, eventID)

	body := fixture.body()
	body["recovers"] = eventID
	request := fixture.call(body)
	request.Header.Set("X-Task-ID", repairTask)
	testutil.Call(t, testHandler.RequestDecision, request).Want(http.StatusBadRequest)
	if eventCleared(t, eventID) {
		t.Fatal("a failed root's event was cleared")
	}
}
