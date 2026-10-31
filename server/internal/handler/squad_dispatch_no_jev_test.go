package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// Squad dispatch must behave as with Jev routing off, even when the workspace
// is configured for routing "on" and Jev would answer with a confident worker.

func newConfidentJevFixture(t *testing.T) (*jevShadowFixture, *atomic.Int32) {
	t.Helper()
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "on"})
	var calls atomic.Int32
	answer := f.confidentWorkerAnswer(0.95)
	f.serve(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		answer(w, r)
	})
	return f, &calls
}

func (f *jevShadowFixture) assertLeaderDispatch(t *testing.T, issueID pgtype.UUID, calls *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	var leaderTasks, workerTasks, jevComments int
	_ = testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, f.leaderID).Scan(&leaderTasks)
	_ = testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, f.workerID).Scan(&workerTasks)
	_ = testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'jev_route'`, issueID).Scan(&jevComments)
	if leaderTasks != 1 || workerTasks != 0 || jevComments != 0 {
		t.Errorf("leader_tasks=%d worker_tasks=%d jev_route_comments=%d, want 1/0/0", leaderTasks, workerTasks, jevComments)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("Jev API called %d times, want 0", n)
	}
}

func TestSquadAssign_DoesNotRouteThroughJev(t *testing.T) {
	f, calls := newConfidentJevFixture(t)
	res, err := testHandler.IssueService.Create(context.Background(), service.IssueCreateParams{
		WorkspaceID:  f.wsUUID,
		Title:        "No Jev on squad assign",
		Description:  pgtype.Text{String: "Go backend work", Valid: true},
		Status:       "todo",
		Priority:     "high",
		AssigneeType: pgtype.Text{String: "squad", Valid: true},
		AssigneeID:   f.squadID,
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(testUserID),
	}, service.IssueCreateOpts{})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, res.Issue.ID) })

	f.assertLeaderDispatch(t, res.Issue.ID, calls)
	if fresh, _ := testHandler.Queries.GetIssue(context.Background(), res.Issue.ID); fresh.Status != "todo" {
		t.Errorf("issue status = %q, want todo (no Jev promotion)", fresh.Status)
	}
}

func TestSquadMentionComment_DoesNotRouteThroughJev(t *testing.T) {
	f, calls := newConfidentJevFixture(t)
	issue := f.insertIssue(t, "todo")

	content := "Hey @[" + f.squad.Name + "](mention://squad/" + util.UUIDToString(f.squadID) + ") please handle this"
	r := newRequest("POST", "/api/issues/"+util.UUIDToString(issue.ID)+"/comments", map[string]any{"content": content})
	r = withURLParam(r, "id", util.UUIDToString(issue.ID))
	w := httptest.NewRecorder()
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment = %d: %s", w.Code, w.Body.String())
	}
	var resp CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, o := range resp.TriggerOutcomes {
		if o.ReasonCode == "jev_routed" {
			t.Errorf("trigger outcome reason_code = jev_routed, want none: %+v", o)
		}
	}
	f.assertLeaderDispatch(t, issue.ID, calls)
}
