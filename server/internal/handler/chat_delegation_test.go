package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestChatSendRecordsOnlyCommittedHumanDelegation(test *testing.T) {
	for _, path := range []string{"direct", "fork"} {
		for _, outcome := range []string{"committed", "rollback", "disabled"} {
			test.Run(path+"/"+outcome, func(test *testing.T) {
				ctx := context.Background()
				_, sourceID, _, _ := setupDirectChatSession(test, ctx, "Delegation")
				if path == "fork" {
					forkMessage(test, sourceID, testutil.Cols{"role": "user", "content": "Source instruction"})
				}
				handler := *testHandler
				tasks := service.NewTaskService(handler.Queries, handler.TxStarter, handler.Hub, handler.Bus, testHandler.TaskService.Wakeup)
				handler.TaskService = tasks
				tasks.RecordMemberActivity = outcome != "disabled"
				dbfx.Exec(test, "DELETE FROM member_delegation_day WHERE user_id=$1", testUserID)
				dbfx.Cleanup(test, "DELETE FROM member_delegation_day WHERE user_id=$1", testUserID)
				status := http.StatusCreated
				if outcome == "rollback" {
					handler.TxStarter = rollbackOnCommitTxStarter{pool: testPool}
					tasks.TxStarter = handler.TxStarter
					status = http.StatusInternalServerError
				}
				var taskID string
				if path == "direct" {
					request := newRequest(http.MethodPost, "/api/chat-sessions/"+sourceID+"/messages", map[string]any{"content": "Please do this"})
					request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sourceID))
					response := testutil.Call(test, handler.SendChatMessage, request).Want(status)
					if status == http.StatusCreated {
						var sent SendChatMessageResponse
						response.JSON(&sent)
						taskID = sent.TaskID
					}
				} else {
					response := testutil.Call(test, handler.ForkChatSession, forkChatRequest(test, sourceID, "delegation", map[string]any{"message": "Please do this"})).Want(status)
					if status == http.StatusCreated {
						var fork ChatForkResponse
						response.JSON(&fork)
						cleanupChatFork(test, fork.Session.ID)
						taskID = *fork.Message.TaskID
					}
				}
				var days int
				if err := testPool.QueryRow(ctx, "SELECT count(*) FROM member_delegation_day WHERE user_id=$1 AND activity_date=$2::date", testUserID, time.Now().UTC().Format(time.DateOnly)).Scan(&days); err != nil {
					test.Fatal(err)
				}
				wantDays := 0
				if outcome == "committed" {
					wantDays = 1
				}
				if days != wantDays {
					test.Fatalf("%s chat delegation days = %d, want %d", outcome, days, wantDays)
				}
				if outcome != "committed" {
					return
				}
				dbfx.Exec(test, "DELETE FROM member_delegation_day WHERE user_id=$1", testUserID)
				task, err := handler.Queries.GetAgentTask(ctx, parseUUID(taskID))
				if err != nil {
					test.Fatal(err)
				}
				tasks.NotifyDirectChatSent(ctx, task)
				if path == "fork" {
					testutil.Call(test, handler.ForkChatSession, forkChatRequest(test, sourceID, "delegation", map[string]any{"message": "Please do this"})).Want(http.StatusOK)
				}
				if count := dbfx.Count(test, "SELECT count(*) FROM member_delegation_day WHERE user_id=$1", testUserID); count != 0 {
					test.Fatalf("notification or replay recorded %d delegation days", count)
				}
			})
		}
	}
}
