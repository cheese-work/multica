package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6428 (S3a of CHE-6385): the comment list returns each root's thread
// relation for the caller (or for=<member or agent id>) and filters by it.

type threadRelationFixture struct {
	IssueID string
	Other   string // second member (user id)
	Agent   string // agent id

	// Roots by scenario. Each root may have a reply in Reply.
	Mine        string // root by V                           -> with_me
	AgentsOnly  string // agent root + agent reply             -> agents_only
	AskV        string // agent root tags V (ask)              -> with_me, waiting for V
	WithOther   string // member O root + agent reply          -> without_me, not agents_only
	AskO        string // agent root tags O (ask)              -> waiting for O
	FYIV        string // agent root tags V with notify_only   -> with_me unless setting off
	Decision    string // agent root carrying an open decision targeting V
	DecisionReq string
	Reply       map[string]string // root id -> reply id
}

func relMention(kind, id string) string { return "[@x](mention://" + kind + "/" + id + ")" }

func newThreadRelationFixture(t *testing.T) threadRelationFixture {
	t.Helper()
	other := dbfx.User(t, "Relation Other", "relation-other-"+time.Now().Format("150405.000000")+"@multica.ai")
	dbfx.Member(t, testWorkspaceID, other, "member")
	agent := dbfx.Agent(t, "relation-agent", testRuntimeID)
	issueID := dbfx.Issue(t, "thread relation fixture")

	base := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	n := 0
	add := func(parent string, authorType, authorID, content string, notifyOnly *bool) string {
		t.Helper()
		n++
		cols := testutil.Cols{
			"author_type": authorType,
			"author_id":   authorID,
			"created_at":  base.Add(time.Duration(n) * time.Minute),
			"notify_only": notifyOnly,
		}
		if parent != "" {
			cols["parent_id"] = parent
		}
		return dbfx.Comment(t, issueID, content, cols)
	}
	no, yes := false, true
	f := threadRelationFixture{IssueID: issueID, Other: other, Agent: agent, Reply: map[string]string{}}

	f.Mine = add("", "member", testUserID, "my thread", &no)
	f.Reply[f.Mine] = add(f.Mine, "agent", agent, "agent answers me", &no)

	f.AgentsOnly = add("", "agent", agent, "agents talk", &no)
	f.Reply[f.AgentsOnly] = add(f.AgentsOnly, "agent", agent, "agents keep talking", &no)

	f.AskV = add("", "agent", agent, "please look "+relMention("member", testUserID), &no)
	f.Reply[f.AskV] = add(f.AskV, "agent", agent, "still waiting", &no)

	f.WithOther = add("", "member", other, "other member thread", &no)
	f.Reply[f.WithOther] = add(f.WithOther, "agent", agent, "agent replies to other", &no)

	f.AskO = add("", "agent", agent, "please look "+relMention("member", other), &no)
	f.Reply[f.AskO] = add(f.AskO, "agent", agent, "waiting on other", &no)

	f.FYIV = add("", "agent", agent, "fyi "+relMention("member", testUserID), &yes)
	f.Reply[f.FYIV] = add(f.FYIV, "agent", agent, "fyi follow up", &no)

	f.Decision = add("", "agent", agent, "decision card", &no)
	f.Reply[f.Decision] = add(f.Decision, "agent", agent, "decision follow up", &no)
	dbfx.QueryRow(t, `INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, ask, escalation_class, origin, issue_comment_id)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), 'Pick', 'reversible', 'asked', $5) RETURNING id::text`,
		testWorkspaceID, issueID, agent, testUserID, f.Decision).Scan(&f.DecisionReq)
	dbfx.Exec(t, `UPDATE comment SET decision_request_id = $1 WHERE id = $2`, f.DecisionReq, f.Decision)
	return f
}

func relationOf(t *testing.T, rows []CommentResponse, root string) ThreadRelationResponse {
	t.Helper()
	got, ok := byID(rows)[root]
	if !ok {
		t.Fatalf("root %s missing from response", root)
	}
	if got.ThreadRelation == nil {
		t.Fatalf("root %s has no thread_relation", root)
	}
	return *got.ThreadRelation
}

func TestListComments_ThreadRelationOnEveryRootForCaller(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newThreadRelationFixture(t)

	_, rows := listComments(t, fx.IssueID, "")
	if len(rows) != 14 {
		t.Fatalf("default list returned %d comments, want 14 (existing calls unchanged)", len(rows))
	}
	want := map[string]ThreadRelationResponse{
		fx.Mine:       {WithMe: true},
		fx.AgentsOnly: {AgentsOnly: true},
		fx.AskV:       {WithMe: true, WaitingOnMe: true},
		fx.WithOther:  {},
		fx.AskO:       {AgentsOnly: true},
		fx.FYIV:       {WithMe: true},
		fx.Decision:   {WithMe: true, WaitingOnMe: true},
	}
	for root, rel := range want {
		if got := relationOf(t, rows, root); got != rel {
			t.Errorf("root %s: got %+v, want %+v", root, got, rel)
		}
	}
	for _, c := range rows {
		if c.ParentID != nil && c.ThreadRelation != nil {
			t.Errorf("reply %s carries thread_relation", c.ID)
		}
	}
}

func TestListComments_RelationWaitingReturnsOnlyWaitingRoots(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newThreadRelationFixture(t)

	_, rows := listComments(t, fx.IssueID, "relation=waiting")
	eqIDs(t, ids(rows), []string{fx.AskV, fx.Reply[fx.AskV], fx.Decision, fx.Reply[fx.Decision]}, "relation=waiting")

	_, rows = listComments(t, fx.IssueID, "relation=waiting&roots_only=true")
	eqIDs(t, ids(rows), []string{fx.AskV, fx.Decision}, "relation=waiting roots_only")
}

func TestListComments_RelationWithMeAndWithoutMe(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newThreadRelationFixture(t)

	_, rows := listComments(t, fx.IssueID, "relation=with_me&roots_only=true")
	eqIDs(t, ids(rows), []string{fx.Mine, fx.AskV, fx.FYIV, fx.Decision}, "with_me")

	_, rows = listComments(t, fx.IssueID, "relation=without_me&roots_only=true")
	eqIDs(t, ids(rows), []string{fx.AgentsOnly, fx.WithOther, fx.AskO}, "without_me")
}

func TestListComments_RelationForAnotherMember(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newThreadRelationFixture(t)

	_, rows := listComments(t, fx.IssueID, "for="+fx.Other)
	if got := relationOf(t, rows, fx.AskO); !got.WithMe || !got.WaitingOnMe {
		t.Errorf("for=other, AskO: got %+v, want with_me and waiting_on_me", got)
	}
	if got := relationOf(t, rows, fx.WithOther); !got.WithMe {
		t.Errorf("for=other, WithOther: got %+v, want with_me", got)
	}
	if got := relationOf(t, rows, fx.AskV); got.WithMe || got.WaitingOnMe {
		t.Errorf("for=other, AskV: got %+v, want without_me", got)
	}

	_, rows = listComments(t, fx.IssueID, "for="+fx.Other+"&relation=waiting&roots_only=true")
	eqIDs(t, ids(rows), []string{fx.AskO}, "for=other waiting")
}

func TestListComments_RelationForAgent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newThreadRelationFixture(t)

	_, rows := listComments(t, fx.IssueID, "for="+fx.Agent+"&relation=with_me&roots_only=true")
	// The agent wrote in every thread except the two member-only roots it did not reply to.
	eqIDs(t, ids(rows), []string{fx.Mine, fx.AgentsOnly, fx.AskV, fx.WithOther, fx.AskO, fx.FYIV, fx.Decision}, "for=agent with_me")
}

func TestListComments_RelationRejectsBadParams(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newThreadRelationFixture(t)
	for _, q := range []string{
		"relation=nonsense",
		"for=not-a-uuid",
		"for=00000000-0000-0000-0000-000000000000",
	} {
		if w, _ := listComments(t, fx.IssueID, q); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, w.Code)
		}
	}
}

func TestListComments_RelationUnreadableIssueIsNotFound(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	otherWS := dbfx.Workspace(t, "relation other ws", "relation-other-ws-"+time.Now().Format("150405.000000"))
	foreign := dbfx.Issue(t, "foreign issue", testutil.Cols{"workspace_id": otherWS})

	for _, id := range []string{foreign, "00000000-0000-0000-0000-000000000000"} {
		for _, q := range []string{"", "relation=waiting", "for=" + testUserID} {
			w, _ := listComments(t, id, q)
			if w.Code != http.StatusNotFound {
				t.Errorf("issue %s query %q: status %d, want 404", id, q, w.Code)
			}
		}
	}
}

func TestListComments_RelationReadFailureReturnsCommentsWithoutField(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newThreadRelationFixture(t)

	testHandler.threadRelationsForTest = func(context.Context, threadRelationQuery) (threadRelationSet, error) {
		return threadRelationSet{}, errors.New("relation read failed")
	}
	t.Cleanup(func() { testHandler.threadRelationsForTest = nil })

	for _, q := range []string{"", "relation=waiting"} {
		w, rows := listComments(t, fx.IssueID, q)
		if w.Code != http.StatusOK {
			t.Fatalf("query %q: status %d, want 200", q, w.Code)
		}
		if len(rows) != 14 {
			t.Fatalf("query %q: %d comments, want all 14", q, len(rows))
		}
		for _, c := range rows {
			if c.ThreadRelation != nil {
				t.Fatalf("query %q: comment %s carries thread_relation after a failed read", q, c.ID)
			}
		}
		var raw []map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		for _, c := range raw {
			if _, has := c["thread_relation"]; has {
				t.Fatalf("query %q: wire body still has thread_relation", q)
			}
		}
	}
}

func TestListComments_FYICountsAsWithMeMemberSetting(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newThreadRelationFixture(t)

	_, rows := listComments(t, fx.IssueID, "")
	if got := relationOf(t, rows, fx.FYIV); !got.WithMe || got.WaitingOnMe {
		t.Fatalf("default setting: got %+v, want with_me, not waiting", got)
	}

	dbfx.Exec(t, `INSERT INTO member_setting (workspace_id, user_id, key, value) VALUES ($1, $2, 'thread_view', '{"fyi_counts_as_with_me": false}'::jsonb)
		ON CONFLICT (workspace_id, user_id, key) DO UPDATE SET value = EXCLUDED.value`, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member_setting WHERE workspace_id = $1 AND user_id = $2 AND key = 'thread_view'`, testWorkspaceID, testUserID)
	})

	_, rows = listComments(t, fx.IssueID, "")
	if got := relationOf(t, rows, fx.FYIV); got.WithMe {
		t.Fatalf("fyi_counts_as_with_me=false: got %+v, want without_me", got)
	}
}
