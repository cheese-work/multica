package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CHE-1418 acceptance 5: a member comment on a `blocked` squad-assigned issue
// wakes the squad leader once. CHE-1048 stayed silent for 6.5h because the
// member's direction change was a reply under a worker comment, and that route
// never reaches the leader.
func TestCreateComment_MemberCommentOnBlockedSquadIssueWakesLeader_CHE1418(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name          string
		status        string
		archiveWorker bool
		replyToWorker bool
		wantLeader    int
		wantWorker    int
	}{
		{name: "reply to unavailable worker on blocked issue", status: "blocked", archiveWorker: true, replyToWorker: true, wantLeader: 1},
		{name: "reply to live worker on blocked issue", status: "blocked", replyToWorker: true, wantLeader: 1, wantWorker: 1},
		{name: "top-level comment on blocked issue", status: "blocked", wantLeader: 1},
		{name: "reply to unavailable worker on in_progress issue", status: "in_progress", archiveWorker: true, replyToWorker: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fx := newSquadCommentTriggerFixture(t)
			issueID := uuidToString(fx.Issue.ID)
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
				testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID)
			})
			if _, err := testPool.Exec(ctx, `UPDATE issue SET status = $2 WHERE id = $1`, issueID, tc.status); err != nil {
				t.Fatalf("set status: %v", err)
			}
			body := map[string]any{"content": "change the approach"}
			if tc.replyToWorker {
				var workerCommentID string
				if err := testPool.QueryRow(ctx, `
					INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
					VALUES ($1, $2, 'agent', $3, 'blocked on a question', 'comment')
					RETURNING id
				`, issueID, testWorkspaceID, fx.OtherID).Scan(&workerCommentID); err != nil {
					t.Fatalf("seed worker comment: %v", err)
				}
				body["parent_id"] = workerCommentID
			}
			if tc.archiveWorker {
				if _, err := testPool.Exec(ctx, `UPDATE agent SET archived_at = now() WHERE id = $1`, fx.OtherID); err != nil {
					t.Fatalf("archive worker: %v", err)
				}
			}

			w := httptest.NewRecorder()
			r := withURLParam(newRequest("POST", "/api/issues/"+issueID+"/comments", body), "id", issueID)
			testHandler.CreateComment(w, r)
			if w.Code != http.StatusCreated {
				t.Fatalf("CreateComment: expected 201, got %d: %s", w.Code, w.Body.String())
			}

			count := func(agentID string) int {
				var n int
				if err := testPool.QueryRow(ctx, `
					SELECT count(*) FROM agent_task_queue
					WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
				`, issueID, agentID).Scan(&n); err != nil {
					t.Fatalf("count tasks: %v", err)
				}
				return n
			}
			if got := count(fx.LeaderID); got != tc.wantLeader {
				t.Fatalf("leader runs: got %d, want %d", got, tc.wantLeader)
			}
			if got := count(fx.OtherID); got != tc.wantWorker {
				t.Fatalf("worker runs: got %d, want %d", got, tc.wantWorker)
			}
		})
	}
}
