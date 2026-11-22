package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6429 (S4 of CHE-6385, plan r3 T5a): `--from-thread` opens a short ask
// thread that links back to its source thread.

type fromThreadFixture struct {
	askGuardCommentFixture
	sourceRoot  string // root of the source thread
	sourceReply string // a reply in the source thread
}

func newFromThreadFixture(t *testing.T) fromThreadFixture {
	t.Helper()
	f := fromThreadFixture{askGuardCommentFixture: newAskGuardCommentFixture(t)}
	f.sourceRoot = f.insertComment(t, f.issueID, "", "source root")
	f.sourceReply = f.insertComment(t, f.issueID, f.sourceRoot, "source reply")
	return f
}

func (f fromThreadFixture) insertComment(t *testing.T, issueID, parentID, content string) string {
	t.Helper()
	id := uuid.NewString()
	var parent any
	if parentID != "" {
		parent = parentID
	}
	dbfx.Exec(t, `INSERT INTO comment (id, issue_id, workspace_id, author_type, author_id, content, type, parent_id)
		VALUES ($1, $2, $3, 'agent', $4, $5, 'comment', $6)`, id, issueID, testWorkspaceID, f.agentID, content, parent)
	return id
}

func (f fromThreadFixture) ask(t *testing.T, fromThread string) *testutil.Response {
	t.Helper()
	return f.post(t, map[string]any{"content": "Asking " + f.mention() + " one thing.", "from_thread": fromThread})
}

type createdAsk struct {
	ID string `json:"id"`
}

func (f fromThreadFixture) askOK(t *testing.T, fromThread string) string {
	t.Helper()
	var created createdAsk
	f.ask(t, fromThread).Want(http.StatusCreated).JSON(&created)
	return created.ID
}

func TestFromThreadStoresSourceRootAndWritesStubInOneCommit(t *testing.T) {
	f := newFromThreadFixture(t)

	// The source is a reply: the server stores the thread ROOT, not the reply.
	askID := f.askOK(t, f.sourceReply)

	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND parent_id IS NULL AND ask_source_root_id = $2`, askID, f.sourceRoot); n != 1 {
		t.Fatalf("new root with ask_source_root_id = source root: %d rows, want 1", n)
	}
	want := "Asked Human in thread " + askID + "."
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND parent_id = $2 AND author_type = 'system' AND content = $3`, f.issueID, f.sourceRoot, want); n != 1 {
		t.Fatalf("stub %q in the source thread: %d rows, want 1", want, n)
	}
	// One transaction: now() is the transaction start, so both rows share created_at.
	if n := dbfx.Count(t, `SELECT count(DISTINCT created_at) FROM comment WHERE id = $1 OR (parent_id = $2 AND author_type = 'system')`, askID, f.sourceRoot); n != 1 {
		t.Errorf("ask root and stub share %d distinct created_at values, want 1 (one commit)", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND ask_source_root_id IS NOT NULL`, f.issueID); n != 1 {
		t.Errorf("comments with a source = %d, want 1 (the stub carries none)", n)
	}
}

func TestFromThreadRootAsSourceStoresThatRoot(t *testing.T) {
	f := newFromThreadFixture(t)
	askID := f.askOK(t, f.sourceRoot)
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND ask_source_root_id = $2`, askID, f.sourceRoot); n != 1 {
		t.Fatalf("ask_source_root_id for a root source: %d rows, want 1", n)
	}
}

func TestFromThreadStubNamesEveryMentionAndStartsNoRun(t *testing.T) {
	f := newFromThreadFixture(t)
	runs := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, f.issueID)
	}
	before := runs()

	askID := f.askOK(t, f.sourceRoot)

	var stubID string
	dbfx.QueryRow(t, `SELECT id FROM comment WHERE parent_id = $1 AND author_type = 'system'`, f.sourceRoot).Scan(&stubID)
	if stubID == "" {
		t.Fatal("no stub in the source thread")
	}
	if after := runs(); after != before {
		t.Errorf("agent_task_queue rows: before %d, after %d; the ask must start no run", before, after)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, stubID); n != 0 {
		t.Errorf("runs triggered by the stub = %d, want 0", n)
	}
	var stub string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE id = $1`, stubID).Scan(&stub)
	if strings.Contains(stub, "mention://") {
		t.Errorf("stub carries a mention link: %q", stub)
	}
	if !strings.Contains(stub, askID) {
		t.Errorf("stub %q does not name the new thread %s", stub, askID)
	}
}

func TestFromThreadLeavesSourceResolutionAlone(t *testing.T) {
	f := newFromThreadFixture(t)
	dbfx.Exec(t, `UPDATE comment SET resolved_at = now(), resolved_by_type = 'member', resolved_by_id = $2 WHERE id = $1`, f.sourceRoot, testUserID)

	f.askOK(t, f.sourceRoot)

	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND resolved_at IS NOT NULL`, f.sourceRoot); n != 1 {
		t.Fatalf("source thread resolution changed: resolved rows = %d, want 1", n)
	}
}

func TestFromThreadRefusesACommentOnAnotherIssue(t *testing.T) {
	f := newFromThreadFixture(t)
	otherIssue := dbfx.Issue(t, "other issue")
	foreign := f.insertComment(t, otherIssue, "", "foreign root")
	before := f.comments(t)

	res := f.ask(t, foreign).Want(http.StatusBadRequest)

	if !strings.Contains(res.Text(), "thread_issue_mismatch") {
		t.Errorf("400 body lacks code thread_issue_mismatch: %s", res.Text())
	}
	if after := f.comments(t); after != before {
		t.Errorf("refused ask saved %d rows", after-before)
	}
}

func TestFromThreadRefusesParentAndUnknownSource(t *testing.T) {
	f := newFromThreadFixture(t)
	before := f.comments(t)

	f.post(t, map[string]any{"content": "x", "from_thread": f.sourceRoot, "parent_id": f.sourceRoot}).Want(http.StatusBadRequest)
	f.ask(t, "not-a-uuid").Want(http.StatusBadRequest)
	f.ask(t, uuid.NewString()).Want(http.StatusBadRequest)

	if after := f.comments(t); after != before {
		t.Errorf("refused asks saved %d rows", after-before)
	}
}

// A task started by a comment may only reply under that comment, except for an
// ask: it opens a new root on purpose.
func TestFromThreadAllowedInACommentTriggeredTask(t *testing.T) {
	f := newFromThreadFixture(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET trigger_comment_id = $2 WHERE id = $1`, f.taskID, f.sourceReply)

	f.post(t, map[string]any{"content": "plain top-level"}).Want(http.StatusConflict)
	f.ask(t, f.sourceRoot).Want(http.StatusCreated)
}

func (f fromThreadFixture) resolveAs(t *testing.T, commentID string, source string) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/comments/"+commentID+"/resolve", nil), "commentId", commentID)
	if source == "task_token" {
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Agent-ID", f.agentID)
		req.Header.Set("X-Task-ID", f.taskID)
	}
	return testutil.Call(t, testHandler.ResolveComment, req)
}

const askRootNotResolvableText = "This comment opens an ask thread; it cannot resolve itself. Resolve the source thread %s with a reply there."

func TestResolveAskRootByAgentTaskIsRefused(t *testing.T) {
	f := newFromThreadFixture(t)
	askID := f.askOK(t, f.sourceReply)

	res := f.resolveAs(t, askID, "task_token").Want(http.StatusConflict)

	if !strings.Contains(res.Text(), "ask_root_not_resolvable") {
		t.Errorf("409 body lacks code ask_root_not_resolvable: %s", res.Text())
	}
	if want := strings.Replace(askRootNotResolvableText, "%s", f.sourceRoot, 1); !strings.Contains(res.Text(), want) {
		t.Errorf("409 body lacks the exact text %q: %s", want, res.Text())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND resolved_at IS NOT NULL`, askID); n != 0 {
		t.Errorf("refused resolve still resolved the ask root")
	}
}

func TestResolveAskRootByMemberSucceeds(t *testing.T) {
	f := newFromThreadFixture(t)
	askID := f.askOK(t, f.sourceRoot)

	f.resolveAs(t, askID, "member").Want(http.StatusOK)

	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND resolved_at IS NOT NULL`, askID); n != 1 {
		t.Errorf("member resolve left the ask root open")
	}
}

func TestResolveReplyInAskThreadByAgentTaskSucceeds(t *testing.T) {
	f := newFromThreadFixture(t)
	askID := f.askOK(t, f.sourceRoot)
	reply := f.insertComment(t, f.issueID, askID, "answer")

	f.resolveAs(t, reply, "task_token").Want(http.StatusOK)
}

func TestResolveSourceRootByAgentTaskStillWorks(t *testing.T) {
	f := newFromThreadFixture(t)
	f.askOK(t, f.sourceRoot)

	f.resolveAs(t, f.sourceRoot, "task_token").Want(http.StatusOK)
}
