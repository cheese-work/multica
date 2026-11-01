package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-1918 (CHE-1917 Slice 0a): a comment that falls back to a squad leader
// writes one durable run_gate_event row. Capture only: the leader path behaves
// exactly as before, and a failed ledger write never changes it.

type gateComment struct {
	issueID, squadID, leaderID, leaderRuntime string
}

func newGateComment(t *testing.T, name string) gateComment {
	t.Helper()
	runtimeID := dbfx.Runtime(t, name+" runtime")
	leaderID := dbfx.Agent(t, name+" leader", runtimeID, testutil.Cols{"max_concurrent_tasks": 3})
	squadID := dbfx.Squad(t, name+" squad", leaderID)
	issueID := dbfx.Issue(t, name+" issue", testutil.Cols{"status": "in_progress", "assignee_type": "squad", "assignee_id": squadID})
	t.Cleanup(func() { dbfx.Exec(t, "DELETE FROM run_gate_event WHERE issue_id = $1", issueID) })
	return gateComment{issueID: issueID, squadID: squadID, leaderID: leaderID, leaderRuntime: runtimeID}
}

func (g gateComment) post(t *testing.T, content string) CommentResponse {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+g.issueID+"/comments", map[string]any{"content": content}), "id", g.issueID)
	var response CommentResponse
	testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated).JSON(&response)
	return response
}

type gateEvent struct {
	origin, authorType, agentID, squadID, status string
	admissionStatus, admissionReason, activeTask *string
	foldTask                                     *string
	eligible, nonLeaderReply                     bool
	revision                                     int64
	seen                                         int
	snapshot                                     []byte
	timeMatches                                  bool
}

func readGateEvent(t *testing.T, commentID string) gateEvent {
	t.Helper()
	var e gateEvent
	dbfx.QueryRow(t, `SELECT e.origin, e.author_type, e.agent_id::text, e.squad_id::text, e.issue_status, e.admission_status, e.admission_reason,
		e.active_task_id::text, e.native_fold_task_id::text, e.native_fold_eligible, e.non_leader_agent_reply, e.issue_revision, e.seen_count,
		e.queue_snapshot, e.event_time = c.created_at
		FROM run_gate_event e JOIN comment c ON c.id = e.trigger_comment_id
		WHERE e.source = 'comment_leader' AND e.event_key = $1`, commentID).Scan(
		&e.origin, &e.authorType, &e.agentID, &e.squadID, &e.status, &e.admissionStatus, &e.admissionReason,
		&e.activeTask, &e.foldTask, &e.eligible, &e.nonLeaderReply, &e.revision, &e.seen, &e.snapshot, &e.timeMatches)
	return e
}

func TestRunGateCapturesLeaderFallbackComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	g := newGateComment(t, "gate fallback")
	body := "gate-body-" + g.issueID

	first := g.post(t, body+" one")
	e := readGateEvent(t, first.ID)
	if e.origin != "issue_assignee" || e.authorType != "member" || e.nonLeaderReply || e.agentID != g.leaderID || e.squadID != g.squadID || e.status != "in_progress" {
		t.Fatalf("first event = %+v", e)
	}
	if !e.timeMatches || e.eligible || e.foldTask != nil || e.activeTask != nil || string(e.snapshot) != "[]" {
		t.Fatalf("first event must see an empty queue at the comment's own time: %+v", e)
	}
	if e.admissionStatus == nil || *e.admissionStatus != "queued" {
		t.Fatalf("admission = %v, want the producer's own queued", e.admissionStatus)
	}
	var queued string
	dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND trigger_comment_id = $3`, g.issueID, g.leaderID, first.ID).Scan(&queued)

	second := g.post(t, body+" two")
	e = readGateEvent(t, second.ID)
	if !e.eligible || e.foldTask == nil || *e.foldTask != queued {
		t.Fatalf("second event must see the queued leader run %s as a native fold target: %+v", queued, e)
	}
	var snapshot []map[string]any
	if err := json.Unmarshal(e.snapshot, &snapshot); err != nil || len(snapshot) != 1 || snapshot[0]["id"] != queued || snapshot[0]["status"] != "queued" {
		t.Fatalf("queue snapshot = %s (%v)", e.snapshot, err)
	}
	if e.admissionStatus == nil || *e.admissionStatus != "coalesced" {
		t.Fatalf("admission = %v, want the producer's own coalesced", e.admissionStatus)
	}

	// Never retain comment text.
	if n := dbfx.Count(t, `SELECT count(*) FROM run_gate_event WHERE issue_id = $1 AND row_to_json(run_gate_event)::text LIKE '%' || $2 || '%'`, g.issueID, body); n != 0 {
		t.Fatalf("%d ledger rows carry comment text", n)
	}
}

func TestRunGateRecordsTheActiveLeaderRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	g := newGateComment(t, "gate active")
	active := dbfx.Task(t, g.leaderID, testutil.Cols{"runtime_id": g.leaderRuntime, "issue_id": g.issueID, "status": "running",
		"started_at": testutil.Raw("now()"), "is_leader_task": true, "squad_id": g.squadID})
	c := g.post(t, "while the leader is running")
	e := readGateEvent(t, c.ID)
	if e.activeTask == nil || *e.activeTask != active || e.eligible {
		t.Fatalf("event = %+v, want active run %s and no queued fold target", e, active)
	}
	if e.admissionStatus == nil || e.admissionReason == nil {
		t.Fatalf("event lost the producer's disposition: %+v", e)
	}
}

func TestRunGateRepeatedTriggerKeepsOneRow(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	g := newGateComment(t, "gate repeat")
	c := g.post(t, "edited later")
	comment, err := testHandler.Queries.GetComment(context.Background(), parseUUID(c.ID))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(g.issueID))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.triggerTasksForComment(context.Background(), issue, comment, nil, "member", testUserID, testUserID, nil, commentSteerRequest{})
	if e := readGateEvent(t, c.ID); e.seen != 2 {
		t.Fatalf("seen_count = %d, want 2 for the same comment triggered twice", e.seen)
	}
}

// Concurrent triggers of one comment race on the unique key; none may fail or
// split the row.
func TestRunGateConcurrentTriggersShareOneRow(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	g := newGateComment(t, "gate race")
	c := g.post(t, "raced")
	comment, err := testHandler.Queries.GetComment(context.Background(), parseUUID(c.ID))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(g.issueID))
	if err != nil {
		t.Fatal(err)
	}
	const racers = 6
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testHandler.triggerTasksForComment(context.Background(), issue, comment, nil, "member", testUserID, testUserID, nil, commentSteerRequest{})
		}()
	}
	wg.Wait()
	if n := dbfx.Count(t, `SELECT count(*) FROM run_gate_event WHERE issue_id = $1`, g.issueID); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if e := readGateEvent(t, c.ID); e.seen != racers+1 {
		t.Fatalf("seen_count = %d, want %d (the post plus every racer)", e.seen, racers+1)
	}
}

func TestRunGateCaptureFailureDoesNotChangeTheLeaderPath(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	g := newGateComment(t, "gate failure")
	dbfx.Exec(t, `CREATE FUNCTION run_gate_test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'run gate test failure'; END $$`)
	t.Cleanup(func() { dbfx.Exec(t, `DROP FUNCTION run_gate_test_fail() CASCADE`) })
	dbfx.Exec(t, fmt.Sprintf(`CREATE TRIGGER run_gate_test_fail BEFORE INSERT ON run_gate_event FOR EACH ROW WHEN (NEW.issue_id = '%s') EXECUTE FUNCTION run_gate_test_fail()`, g.issueID))

	c := g.post(t, "the ledger is down")
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND trigger_comment_id = $3`, g.issueID, g.leaderID, c.ID); n != 1 {
		t.Fatalf("leader runs for the comment = %d, want the usual 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM run_gate_event WHERE issue_id = $1`, g.issueID); n != 0 {
		t.Fatalf("ledger rows = %d, want 0 when the write failed", n)
	}
}

// An agent's unmentioned reply is suppressed before leader-fallback capture.
func TestRunGateDoesNotCaptureSuppressedWorkerReply(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	g := newGateComment(t, "gate worker")
	workerRuntimeID := dbfx.Runtime(t, "gate worker runtime")
	workerID := dbfx.Agent(t, "gate worker", workerRuntimeID)
	dbfx.SquadMember(t, g.squadID, "agent", workerID)
	sourceID := dbfx.Task(t, g.leaderID, testutil.Cols{"runtime_id": g.leaderRuntime, "issue_id": g.issueID, "status": "completed",
		"is_leader_task": true, "squad_id": g.squadID, "originator_user_id": testUserID, "accountable_user_id": testUserID})
	rootID := dbfx.Comment(t, g.issueID, fmt.Sprintf("[@Worker](mention://agent/%s) verify", workerID), testutil.Cols{
		"author_type": "agent", "author_id": g.leaderID, "source_task_id": sourceID})
	workerTaskID := dbfx.Task(t, workerID, testutil.Cols{"runtime_id": workerRuntimeID, "issue_id": g.issueID, "status": "running",
		"trigger_comment_id": rootID, "squad_id": g.squadID, "delegated_from_task_id": sourceID,
		"originator_user_id": testUserID, "accountable_user_id": testUserID})
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+g.issueID+"/comments", map[string]any{"content": "Verification complete", "parent_id": rootID}), "id", g.issueID)
	req.Header.Set("X-Agent-ID", workerID)
	req.Header.Set("X-Task-ID", workerTaskID)
	var reply CommentResponse
	testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated).JSON(&reply)

	if len(reply.TriggerOutcomes) != 1 || reply.TriggerOutcomes[0].Status != DispatchBlocked || reply.TriggerOutcomes[0].ReasonCode != ReasonAgentCommentImplicitRouteSuppressed {
		t.Fatalf("worker reply outcomes = %+v, want implicit route suppression", reply.TriggerOutcomes)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM run_gate_event WHERE issue_id = $1 AND trigger_comment_id = $2`, g.issueID, reply.ID); n != 0 {
		t.Fatalf("suppressed worker reply wrote %d run-gate events", n)
	}
}
