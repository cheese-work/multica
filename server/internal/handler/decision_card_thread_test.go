package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// setCardThread stores decision_card_thread for the test workspace.
func setCardThread(t *testing.T, value string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = COALESCE(settings, '{}') || jsonb_build_object('decision_card_thread', $1::text) WHERE id = $2`, value, testWorkspaceID); err != nil {
		t.Fatalf("set decision_card_thread: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = settings - 'decision_card_thread' WHERE id = $1`, testWorkspaceID)
	})
}

type askRow struct {
	id, content             string
	parentID, askSourceRoot *string
}

func askCommentOf(t *testing.T, issueID string) askRow {
	t.Helper()
	var row askRow
	if err := testPool.QueryRow(context.Background(), `SELECT id::text, content, parent_id::text, ask_source_root_id::text FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'ask'`, issueID).Scan(&row.id, &row.content, &row.parentID, &row.askSourceRoot); err != nil {
		t.Fatalf("read ask comment: %v", err)
	}
	return row
}

func TestDecisionCardThreadMention(t *testing.T) {
	humanMention := func(fixture singleCallFixture) string { return "mention://member/" + fixture.humanID + ")" }
	t.Run("on adds the mention whatever the lint mode holds", func(t *testing.T) {
		setCardThread(t, "on")
		setCardLint(t, "off")
		fixture := newSingleCallFixture(t)
		testutil.Call(t, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated)
		if got := askCommentOf(t, fixture.issueID).content; !strings.Contains(got, humanMention(fixture)) {
			t.Errorf("ask comment lacks the human mention: %s", got)
		}
	})
	t.Run("off keeps the comment as written", func(t *testing.T) {
		setCardThread(t, "off")
		setCardLint(t, "off")
		fixture := newSingleCallFixture(t)
		testutil.Call(t, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated)
		if got := askCommentOf(t, fixture.issueID).content; strings.Contains(got, humanMention(fixture)) {
			t.Errorf("off must not add a mention: %s", got)
		}
	})
	t.Run("on adds the mention on the intent path too", func(t *testing.T) {
		setCardThread(t, "on")
		setCardLint(t, "off")
		setCannotDo(t, "off", "")
		fixture := newRequestCoreFixture(t, false)
		testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, fixture.newShapeAsk())).Want(http.StatusCreated)
		if got := askCommentOf(t, fixture.issueID).content; !strings.Contains(got, "mention://member/"+fixture.humanID+")") {
			t.Errorf("ask comment lacks the human mention: %s", got)
		}
	})
}

// The added mention must not create a second Inbox item: the decision ping is
// the only one, and the comment event keeps the target in decision_intent_targets
// so the notification listener skips the mention row for that person.
func TestDecisionCardThreadOneInboxItemPerAsk(t *testing.T) {
	setCardThread(t, "on")
	fixture := newSingleCallFixture(t)
	var mu sync.Mutex
	var targets []string
	testHandler.Bus.Subscribe(protocol.EventCommentCreated, func(event events.Event) {
		payload, _ := event.Payload.(map[string]any)
		if list, ok := payload["decision_intent_targets"].([]string); ok {
			mu.Lock()
			targets = append(targets, list...)
			mu.Unlock()
		}
	})
	testutil.Call(t, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated)
	if _, _, pings := fixture.counts(t); pings != 1 {
		t.Errorf("inbox items for the asked member = %d, want 1", pings)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(targets) != 1 || targets[0] != fixture.humanID {
		t.Errorf("decision_intent_targets = %v, want [%s]", targets, fixture.humanID)
	}
}

func TestDecisionCardThreadParentBecomesSource(t *testing.T) {
	newParent := func(t *testing.T, fixture singleCallFixture) string {
		return dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "member", "author_id": testUserID, "content": "root", "type": "comment", "resolved_at": time.Now(), "resolved_by_type": "member", "resolved_by_id": testUserID})
	}
	t.Run("on: new root with the source and a stub", func(t *testing.T) {
		setCardThread(t, "on")
		fixture := newSingleCallFixture(t)
		root := newParent(t, fixture)
		reply := dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "member", "author_id": testUserID, "content": "reply", "type": "comment", "parent_id": root})
		body := fixture.body()
		body["parent_id"] = reply
		var created struct {
			Hints []string `json:"hints"`
		}
		testutil.Call(t, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated).JSON(&created)
		ask := askCommentOf(t, fixture.issueID)
		if ask.parentID != nil {
			t.Errorf("ask parent_id = %s, want a new root", *ask.parentID)
		}
		if ask.askSourceRoot == nil || *ask.askSourceRoot != root {
			t.Errorf("ask_source_root_id = %v, want the thread root %s", ask.askSourceRoot, root)
		}
		var stub string
		if err := testPool.QueryRow(context.Background(), `SELECT content FROM comment WHERE issue_id = $1 AND parent_id = $2 AND author_type = 'system'`, fixture.issueID, root).Scan(&stub); err != nil {
			t.Fatalf("source thread has no stub: %v", err)
		}
		if !strings.HasPrefix(stub, "Asked") || !strings.Contains(stub, "in thread "+ask.id+".") {
			t.Errorf("stub = %q, want it to name thread %s", stub, ask.id)
		}
		want := "Card placed in a new thread " + ask.id + "; thread " + root + " got a link."
		if !strings.Contains(strings.Join(created.Hints, "\n"), want) {
			t.Errorf("hints = %q, want %q", created.Hints, want)
		}
		if dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND resolved_at IS NOT NULL`, root) != 1 {
			t.Error("the source thread lost its resolution")
		}
		if dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'dispatched')`, fixture.issueID) != 0 {
			t.Error("the stub started a run")
		}
	})
	t.Run("on without a parent: a plain new root, no source, no stub", func(t *testing.T) {
		setCardThread(t, "on")
		fixture := newSingleCallFixture(t)
		testutil.Call(t, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated)
		ask := askCommentOf(t, fixture.issueID)
		if ask.parentID != nil || ask.askSourceRoot != nil {
			t.Errorf("parent=%v source=%v, want both empty", ask.parentID, ask.askSourceRoot)
		}
		if dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, fixture.issueID) != 0 {
			t.Error("a card with no parent wrote a stub")
		}
	})
	t.Run("off: today's placement and content", func(t *testing.T) {
		setCardThread(t, "off")
		fixture := newSingleCallFixture(t)
		root := newParent(t, fixture)
		body := fixture.body()
		body["parent_id"] = root
		testutil.Call(t, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
		ask := askCommentOf(t, fixture.issueID)
		if ask.parentID == nil || *ask.parentID != root || ask.askSourceRoot != nil {
			t.Errorf("parent=%v source=%v, want a reply under %s with no source", ask.parentID, ask.askSourceRoot, root)
		}
		if strings.Contains(ask.content, "mention://member/"+fixture.humanID+")") {
			t.Errorf("off must not add a mention: %s", ask.content)
		}
		if dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, fixture.issueID) != 0 {
			t.Error("off wrote a stub")
		}
		if dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND resolved_at IS NULL`, root) != 1 {
			t.Error("off: the reply should clear the parent's resolution, as today")
		}
	})
}

// The two-step path: the decision-intent tag comment is the card's first
// comment, so under decision_card_thread=on it opens a new root too, and the
// thread it replaced gets the stub (T3, plan r3).
func TestDecisionCardThreadIntentTagOpensNewRoot(t *testing.T) {
	setup := func(t *testing.T) (intentFixture, string, string) {
		f := newIntentFixture(t)
		root := dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": f.issueID, "author_type": "member", "author_id": testUserID, "content": "root", "type": "comment", "resolved_at": time.Now(), "resolved_by_type": "member", "resolved_by_id": testUserID})
		reply := dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": f.issueID, "author_type": "member", "author_id": testUserID, "content": "reply", "type": "comment", "parent_id": root})
		return f, root, reply
	}
	type tagRow struct{ parentID, source *string }
	readTag := func(t *testing.T, id string) tagRow {
		var row tagRow
		if err := testPool.QueryRow(context.Background(), `SELECT parent_id::text, ask_source_root_id::text FROM comment WHERE id = $1`, id).Scan(&row.parentID, &row.source); err != nil {
			t.Fatalf("read tag: %v", err)
		}
		return row
	}
	t.Run("on: tag is a new root with the source, a stub and the hint; the ask lands in that thread", func(t *testing.T) {
		setCardThread(t, "on")
		setCardLint(t, "off")
		setCannotDo(t, "off", "")
		f, root, reply := setup(t)
		var tag CommentResponse
		f.post(t, "task_token", f.taskID, f.mention(), map[string]any{"parent_id": reply}).Want(http.StatusCreated).JSON(&tag)
		row := readTag(t, tag.ID)
		if row.parentID != nil {
			t.Errorf("tag parent_id = %s, want a new root", *row.parentID)
		}
		if row.source == nil || *row.source != root {
			t.Errorf("tag ask_source_root_id = %v, want %s", row.source, root)
		}
		var stub string
		if err := testPool.QueryRow(context.Background(), `SELECT content FROM comment WHERE issue_id = $1 AND parent_id = $2 AND author_type = 'system'`, f.issueID, root).Scan(&stub); err != nil {
			t.Fatalf("source thread has no stub: %v", err)
		}
		if !strings.Contains(stub, "in thread "+tag.ID+".") {
			t.Errorf("stub = %q, want it to name thread %s", stub, tag.ID)
		}
		want := "Card placed in a new thread " + tag.ID + "; thread " + root + " got a link."
		if !strings.Contains(strings.Join(tag.Hints, "\n"), want) {
			t.Errorf("hints = %q, want %q", tag.Hints, want)
		}
		if dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND resolved_at IS NOT NULL`, root) != 1 {
			t.Error("the source thread lost its resolution")
		}
		// The request then lands inside the tag's new thread.
		var intentID string
		dbfx.QueryRow(t, `SELECT decision_intent_id FROM comment WHERE id = $1`, tag.ID).Scan(&intentID)
		sessionID := dbfx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": f.agentID, "creator_id": f.humanID, "title": "Decision lane"})
		dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "see [issue](mention://issue/" + f.issueID + ")"})
		issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(f.issueID))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM decision_request WHERE issue_id = $1`, f.issueID)
		})
		core := requestCoreFixture{intentFixture: f, issue: issue, intentID: intentID, tagID: tag.ID, sessionID: sessionID}
		testutil.Call(t, testHandler.RequestDecision, core.askRequest(t, core.newShapeAsk())).Want(http.StatusCreated)
		ask := askCommentOf(t, f.issueID)
		if ask.parentID == nil || *ask.parentID != tag.ID {
			t.Errorf("ask parent_id = %v, want the tag %s (the new thread)", ask.parentID, tag.ID)
		}
	})
	t.Run("on without a parent: the tag is a plain root, no stub", func(t *testing.T) {
		setCardThread(t, "on")
		f, _, _ := setup(t)
		var tag CommentResponse
		f.post(t, "task_token", f.taskID, f.mention(), nil).Want(http.StatusCreated).JSON(&tag)
		if row := readTag(t, tag.ID); row.parentID != nil || row.source != nil {
			t.Errorf("parent=%v source=%v, want both empty", row.parentID, row.source)
		}
		if dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, f.issueID) != 0 {
			t.Error("a tag with no parent wrote a stub")
		}
	})
	t.Run("off: the tag stays a reply under its parent", func(t *testing.T) {
		setCardThread(t, "off")
		f, root, reply := setup(t)
		var tag CommentResponse
		f.post(t, "task_token", f.taskID, f.mention(), map[string]any{"parent_id": reply}).Want(http.StatusCreated).JSON(&tag)
		row := readTag(t, tag.ID)
		if row.parentID == nil || row.source != nil {
			t.Errorf("parent=%v source=%v, want a reply with no source (root %s)", row.parentID, row.source, root)
		}
		if dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, f.issueID) != 0 {
			t.Error("off wrote a stub")
		}
	})
}
