package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6121 (S2 of CHE-6110): the comments of one status-handoff chain share one
// thread, whichever run posts first. Every move goes through the status API
// with the mover's X-Task-ID, so the real issue_handoff_dispatch row is written.

// chainSteps are the statuses whose handler is the next agent of the chain.
var chainSteps = []string{"in_progress", "in_review", "blocked"}

type chainFx struct {
	IssueID string
	Agents  []string // Agents[i] runs Runs[i]
	Runs    []string
	runtime string
}

// newChainFx builds n agents and an issue held by agent 0, whose running task
// is Runs[0]. Moving the issue to chainSteps[i] hands it to agent i+1.
func newChainFx(t *testing.T, n int) *chainFx {
	t.Helper()
	seedTestCatalog(t)
	fx := &chainFx{runtime: handlerTestRuntimeID(t)}
	for i := 0; i < n; i++ {
		fx.Agents = append(fx.Agents, createHandlerTestAgent(t, "che6121 chain "+workflowTestSuffix(), nil))
	}
	projectID := createWorkflowTestProject(t, "che6121 chain "+workflowTestSuffix())
	for i := 1; i < n; i++ {
		setColumnHandoff(t, projectID, chainSteps[i-1], handToAgent(fx.Agents[i], "go")).Want(http.StatusOK)
	}
	fx.IssueID = dbfx.Issue(t, "che6121 chain", testutil.Cols{
		"project_id": projectID, "status": "todo", "assignee_type": "agent", "assignee_id": fx.Agents[0],
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, fx.IssueID)
		testPool.Exec(context.Background(), `DELETE FROM issue_handoff_dispatch WHERE issue_id = $1`, fx.IssueID)
	})
	fx.Runs = []string{fx.runTask(t, fx.Agents[0])}
	return fx
}

func (fx *chainFx) runTask(t *testing.T, agentID string) string {
	t.Helper()
	return dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": fx.runtime, "issue_id": fx.IssueID, "status": "running", "started_at": testutil.Raw("now()"),
	})
}

// move makes agent `from` move the issue into the next chain step through the
// status API. The run that the move starts becomes Runs[from+1].
func (fx *chainFx) move(t *testing.T, from int, extra map[string]any) {
	t.Helper()
	body := map[string]any{"status": chainSteps[from]}
	for k, v := range extra {
		body[k] = v
	}
	headers := map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": fx.Agents[from], "X-Task-ID": fx.Runs[from]}
	issueWriters[0].write(t, fx.IssueID, body, headers).Want(http.StatusOK)
	fx.adoptStartedRun(t, from+1)
}

// memberMove moves the issue as the workspace member; it starts agent 1's run.
func (fx *chainFx) memberMove(t *testing.T) {
	t.Helper()
	issueWriters[0].write(t, fx.IssueID, map[string]any{"status": chainSteps[0]}, nil).Want(http.StatusOK)
	fx.adoptStartedRun(t, 1)
}

func (fx *chainFx) adoptStartedRun(t *testing.T, i int) {
	t.Helper()
	var id string
	dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 ORDER BY created_at DESC LIMIT 1`,
		fx.IssueID, fx.Agents[i]).Scan(&id)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, id)
	fx.Runs = append(fx.Runs[:i], id)
}

// postOn posts a plain comment with no parent as agent i's run, on issueID.
func (fx *chainFx) postOn(t *testing.T, issueID string, i int, content string) (int, map[string]any) {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content}), "id", issueID)
	req.Header.Set("X-Agent-ID", fx.Agents[i])
	req.Header.Set("X-Task-ID", fx.Runs[i])
	rec := testutil.Call(t, testHandler.CreateComment, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (fx *chainFx) post(t *testing.T, i int, content string) (id, parent string) {
	t.Helper()
	code, out := fx.postOn(t, fx.IssueID, i, content)
	if code != http.StatusCreated {
		t.Fatalf("agent %d post = %d, want 201: %v", i, code, out)
	}
	id, _ = out["id"].(string)
	return id, handoffParentOf(out)
}

func wantParent(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: parent_id = %q, want %q", what, got, want)
	}
}

func topLevelCount(t *testing.T, issueID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND parent_id IS NULL AND author_type = 'agent'`, issueID).Scan(&n)
	return n
}

// 1. Comment first: the mover posts, moves, ends; the started run joins.
func TestChainThread_CommentFirstJoinsMoverComment(t *testing.T) {
	fx := newChainFx(t, 2)
	c, _ := fx.post(t, 0, "result of M")
	fx.move(t, 0, nil)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fx.Runs[0])
	_, parent := fx.post(t, 1, "result of T")
	wantParent(t, "T after M ended", parent, c)
}

// 2. Status first: M moves, then posts while still running; T joins M's comment.
func TestChainThread_StatusFirstMoverPostsThenStartedRun(t *testing.T) {
	fx := newChainFx(t, 2)
	fx.move(t, 0, nil)
	c, _ := fx.post(t, 0, "result of M")
	_, parent := fx.post(t, 1, "result of T")
	wantParent(t, "T after M posted", parent, c)
}

// 3. T before M: T's comment is the root; M, still running, joins it.
func TestChainThread_StartedRunFirstMoverJoins(t *testing.T) {
	fx := newChainFx(t, 2)
	fx.move(t, 0, nil)
	r, parent := fx.post(t, 1, "result of T")
	wantParent(t, "T first", parent, "")
	_, parent = fx.post(t, 0, "result of M")
	wantParent(t, "M after T", parent, r)
}

// 4. Three status-first handoffs, no run ended; the comments land out of order
// and still make one thread.
func TestChainThread_ThreeHandoffsOneThread(t *testing.T) {
	fx := newChainFx(t, 4)
	fx.move(t, 0, nil)
	fx.move(t, 1, nil)
	fx.move(t, 2, nil)
	root, parent := fx.post(t, 3, "D")
	wantParent(t, "D first", parent, "")
	for _, i := range []int{1, 0, 2} {
		_, parent = fx.post(t, i, "later")
		wantParent(t, "later chain comment", parent, root)
	}
	if n := topLevelCount(t, fx.IssueID); n != 1 {
		t.Fatalf("agent threads on the issue = %d, want 1", n)
	}
}

// 5. Two first posts at once take turns on the handoff lock: one root, one reply.
func TestChainThread_ConcurrentFirstPostsMakeOneRoot(t *testing.T) {
	fx := newChainFx(t, 2)
	fx.move(t, 0, nil)
	var wg sync.WaitGroup
	raceUntilBlocked(t, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(),
			`SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':handoff_thread', 0))`, fx.IssueID); err != nil {
			t.Fatal(err)
		}
	}, func() {
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fx.postOn(t, fx.IssueID, i, "first post")
			}()
		}
		wg.Wait()
	})
	if n := topLevelCount(t, fx.IssueID); n != 1 {
		t.Fatalf("top-level agent comments = %d, want exactly 1", n)
	}
}

// 6. Guard edge: a decision-request comment is not a chain comment.
func TestChainThread_DecisionCommentIsSkipped(t *testing.T) {
	fx := newChainFx(t, 2)
	eligible := dbfx.Comment(t, fx.IssueID, "M's result", testutil.Cols{
		"author_type": "agent", "author_id": fx.Agents[0], "source_task_id": fx.Runs[0],
	})
	dbfx.Comment(t, fx.IssueID, "decision card", testutil.Cols{
		"author_type": "agent", "author_id": fx.Agents[0], "source_task_id": fx.Runs[0],
		"decision_request_id": testutil.Raw("gen_random_uuid()"), "decision_comment_kind": "ask",
	})
	fx.move(t, 0, nil)
	_, parent := fx.post(t, 1, "result of T")
	wantParent(t, "newest chain comment is a decision card", parent, eligible)
}

func TestChainThread_OnlyDecisionCommentPostsTopLevel(t *testing.T) {
	fx := newChainFx(t, 2)
	dbfx.Comment(t, fx.IssueID, "decision card", testutil.Cols{
		"author_type": "agent", "author_id": fx.Agents[0], "source_task_id": fx.Runs[0],
		"decision_request_id": testutil.Raw("gen_random_uuid()"), "decision_comment_kind": "ask",
	})
	fx.move(t, 0, nil)
	_, parent := fx.post(t, 1, "result of T")
	wantParent(t, "no eligible chain comment", parent, "")
}

// 7. Guard edge: a member move starts a new chain.
func TestChainThread_MemberMoveStartsNewChain(t *testing.T) {
	fx := newChainFx(t, 2)
	fx.post(t, 0, "M before the member move")
	fx.memberMove(t)
	_, parent := fx.post(t, 1, "result of T")
	wantParent(t, "run started by a member move", parent, "")
}

// 8. Cap rollover: a full chain thread sends the next post to a new root.
func TestChainThread_CapRollover(t *testing.T) {
	fx := newChainFx(t, 2)
	root := dbfx.Comment(t, fx.IssueID, "root", testutil.Cols{"author_type": "agent", "author_id": fx.Agents[0], "source_task_id": fx.Runs[0]})
	for i := 0; i < 29; i++ {
		dbfx.Comment(t, fx.IssueID, "reply", testutil.Cols{
			"author_type": "agent", "author_id": fx.Agents[0], "source_task_id": fx.Runs[0], "parent_id": root,
		})
	}
	fx.move(t, 0, nil)
	next, parent := fx.post(t, 1, "T, thread is full")
	wantParent(t, "30 comments in the thread", parent, "")
	_, parent = fx.post(t, 0, "M, after the rollover")
	wantParent(t, "post after the rollover", parent, next)
}

// 9. Guard edge: a chain comment on another issue does not count.
func TestChainThread_OtherIssueCommentIsIgnored(t *testing.T) {
	fx := newChainFx(t, 2)
	other := dbfx.Issue(t, "che6121 other issue", nil)
	if code, out := fx.postOn(t, other, 0, "M elsewhere"); code != http.StatusCreated {
		t.Fatalf("post on the other issue = %d: %v", code, out)
	}
	fx.move(t, 0, nil)
	_, parent := fx.post(t, 1, "result of T")
	wantParent(t, "only comment is on another issue", parent, "")
}

// 10. Guard edge: a move with a handoff_note writes no edge.
func TestChainThread_HandoffNoteMakesNoEdge(t *testing.T) {
	fx := newChainFx(t, 2)
	fx.post(t, 0, "M before the move")
	fx.move(t, 0, map[string]any{"handoff_note": "context for the next run"})
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM issue_handoff_dispatch WHERE issue_id = $1`, fx.IssueID).Scan(&n)
	if n != 0 {
		t.Fatalf("dispatch rows = %d, want 0 for a handoff_note move", n)
	}
	_, parent := fx.post(t, 1, "result of T")
	wantParent(t, "run started by a handoff_note move", parent, "")
}
