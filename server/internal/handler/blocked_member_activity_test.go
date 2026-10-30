package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestBlockedMemberCommentWakesAssignee_CHE2393(t *testing.T) {
	for _, assigneeType := range []string{"agent", "squad"} {
		for _, route := range []string{"other agent thread", "member thread", "explicit other agent", "assignee thread"} {
			for _, pending := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/pending=%t", assigneeType, route, pending), func(t *testing.T) {
					assigneeID := createHandlerTestAgent(t, "blocked assignee", nil)
					otherID := createHandlerTestAgent(t, "thread owner", nil)
					ownerID := assigneeID
					if assigneeType == "squad" {
						ownerID = dbfx.Squad(t, "blocked squad", assigneeID)
					}
					issueID := dbfx.Issue(t, "blocked member input", testutil.Cols{
						"status": "blocked", "assignee_type": assigneeType, "assignee_id": ownerID,
					})
					dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id = $1", issueID)
					body := map[string]any{"content": "Use the revised approach."}
					wantOther := 0
					switch route {
					case "other agent thread":
						body["parent_id"] = dbfx.Comment(t, issueID, "Need a decision.", testutil.Cols{"author_type": "agent", "author_id": otherID})
						wantOther = 1
					case "member thread":
						body["parent_id"] = dbfx.Comment(t, issueID, "Original member question.")
					case "explicit other agent":
						body["content"] = fmt.Sprintf("[@worker](mention://agent/%s) Use the revised approach.", otherID)
						wantOther = 1
					case "assignee thread":
						body["parent_id"] = dbfx.Comment(t, issueID, "Need a decision.", testutil.Cols{"author_type": "agent", "author_id": assigneeID})
					}
					if pending {
						cols := testutil.Cols{"issue_id": issueID, "runtime_id": handlerTestRuntimeID(t)}
						if assigneeType == "squad" {
							cols["squad_id"], cols["is_leader_task"] = ownerID, true
						}
						dbfx.Task(t, assigneeID, cols)
					}
					// Preview and create share the recipient computation.
					preview := previewCommentTriggersForTest(t, issueID, body)
					wantIDs := []string{assigneeID}
					if wantOther == 1 {
						wantIDs = append(wantIDs, otherID)
					}
					for i := 0; i < 2; i++ {
						replyID := postCommentForTriggerPreviewTest(t, issueID, body)
						if got := countQueuedCommentTriggerTasks(t, issueID, assigneeID); got != 1 {
							t.Fatalf("assignee runs = %d after reply %d, want exactly 1", got, i+1)
						}
						var carriesReply bool
						dbfx.QueryRow(t, `SELECT trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids)
							FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`,
							issueID, assigneeID, replyID).Scan(&carriesReply)
						if !carriesReply {
							t.Fatal("pending assignee run does not carry the member reply")
						}
						if got := countQueuedCommentTriggerTasks(t, issueID, otherID); got != wantOther {
							t.Fatalf("thread/mention recipient runs = %d, want %d", got, wantOther)
						}
					}
					requirePreviewAgents(t, preview, wantIDs...)
				})
			}
		}
	}
}

func TestBlockedMemberStatusChangeWakesAssignee_CHE2393(t *testing.T) {
	seedTestCatalog(t)
	for _, writer := range issueWriters {
		for _, assigneeType := range []string{"agent", "squad"} {
			for _, transition := range [][2]string{{"blocked", "in_progress"}, {"in_progress", "blocked"}} {
				for _, pending := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s-to-%s/pending=%t", writer.name, assigneeType, transition[0], transition[1], pending), func(t *testing.T) {
						agentID := createHandlerTestAgent(t, "status assignee", nil)
						ownerID := agentID
						if assigneeType == "squad" {
							ownerID = dbfx.Squad(t, "status squad", agentID)
						}
						issueID := dbfx.Issue(t, "blocked status input", testutil.Cols{
							"status": transition[0], "assignee_type": assigneeType, "assignee_id": ownerID,
						})
						dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id = $1", issueID)
						if pending {
							cols := testutil.Cols{"issue_id": issueID, "runtime_id": handlerTestRuntimeID(t)}
							if assigneeType == "squad" {
								cols["squad_id"], cols["is_leader_task"] = ownerID, true
							}
							dbfx.Task(t, agentID, cols)
						}
						preview := previewIssueTrigger(t, map[string]any{"issue_ids": []string{issueID}, "status": transition[1]})
						wantPreview := 1
						if pending {
							wantPreview = 0
						}
						if preview.TotalCount != wantPreview {
							t.Errorf("status preview = %d, want %d", preview.TotalCount, wantPreview)
						}
						writer.write(t, issueID, map[string]any{"status": transition[1]}, nil).Want(http.StatusOK)
						if got := countQueuedCommentTriggerTasks(t, issueID, agentID); got != 1 {
							t.Fatalf("assignee runs after status change = %d, want exactly 1", got)
						}
						issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
						if err != nil || issue.Status != transition[1] || uuidToString(issue.AssigneeID) != ownerID {
							t.Fatalf("status/owner changed unexpectedly: %+v, %v", issue, err)
						}
					})
				}
			}
		}
	}
}

func TestBlockedMemberStatusChangePreservesSuppression_CHE2393(t *testing.T) {
	seedTestCatalog(t)
	for _, writer := range issueWriters {
		for _, tc := range []struct {
			name, from, to  string
			agent, suppress bool
		}{
			{name: "agent status change", from: "blocked", to: "in_progress", agent: true},
			{name: "explicit suppress", from: "blocked", to: "in_progress", suppress: true},
			{name: "same status", from: "blocked", to: "blocked"},
			{name: "unrelated edit", from: "blocked"},
			{name: "unrelated transition", from: "todo", to: "in_progress"},
			{name: "backlog parking", from: "blocked", to: "backlog"},
			{name: "done", from: "blocked", to: "done"},
			{name: "cancelled", from: "blocked", to: "cancelled"},
		} {
			t.Run(writer.name+"/"+tc.name, func(t *testing.T) {
				agentID := createHandlerTestAgent(t, "suppression assignee", nil)
				issueID := dbfx.Issue(t, "blocked suppression", testutil.Cols{
					"status": tc.from, "assignee_type": "agent", "assignee_id": agentID,
				})
				dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id = $1", issueID)
				body := map[string]any{"title": "updated title"}
				if tc.to != "" {
					body["status"] = tc.to
				}
				if tc.suppress {
					body["suppress_run"] = true
				}
				var headers map[string]string
				if tc.agent {
					taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issueID)
					headers = map[string]string{"X-Agent-ID": agentID, "X-Task-ID": taskID}
				}
				writer.write(t, issueID, body, headers).Want(http.StatusOK)
				if got := countQueuedCommentTriggerTasks(t, issueID, agentID); got != 0 {
					t.Fatalf("suppressed status write queued %d assignee runs, want 0", got)
				}
			})
		}
	}
}
