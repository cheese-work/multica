package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// reserved_chat_slots (CHE-8748): the daemon passes a non-DM budget with each
// batch claim; the server must keep the remaining slots for DM runs. A DM is a
// chat task that is not a group channel conversation.

// queueReservedSlotsWork queues issueTasks issue tasks (one agent and issue
// each) and then one chat task, so FIFO order alone would hand every slot to
// issue work before the chat task.
func (f *claimDecisionFixture) queueReservedSlotsWork(t *testing.T, issueTasks int) {
	t.Helper()
	f.insertTask(t, "queued")
	for i := 1; i < issueTasks; i++ {
		f.addRuntimeAgentWithTask(t, "reserved-slots-issue-agent-"+string(rune('a'+i)))
	}
	chatAgentID := f.fx.Agent(t, "reserved-slots-dm-agent", util.UUIDToString(f.runtimeID), testutil.Cols{
		"owner_id":             f.fx.UserID,
		"max_concurrent_tasks": 5,
	})
	session := f.fx.ChatSession(t, chatAgentID)
	f.fx.Task(t, chatAgentID, testutil.Cols{
		"runtime_id":      util.UUIDToString(f.runtimeID),
		"chat_session_id": session,
		"status":          "queued",
	})
}

func countChat(tasks []db.AgentTaskQueue) (chat, issue int) {
	for _, task := range tasks {
		if task.ChatSessionID.Valid {
			chat++
		} else {
			issue++
		}
	}
	return chat, issue
}

func TestClaimTasksForRuntimesNonDMLimitKeepsSlotsForChat(t *testing.T) {
	cases := []struct {
		name      string
		limit     *int
		wantChat  int
		wantIssue int
		maxTasks  int
	}{
		{"no limit equals base", nil, 1, 3, 4},
		{"limit 2 leaves the last slot for chat", intPtr(2), 1, 2, 4},
		{"budget spent claims chat only", intPtr(0), 1, 0, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newClaimDecisionFixture(t, 5)
			f.queueReservedSlotsWork(t, 3)
			claimed, _, err := f.svc.ClaimTasksForRuntimesWithNonDMLimit(context.Background(), []pgtype.UUID{f.runtimeID}, c.maxTasks, c.limit)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			chat, issue := countChat(claimed)
			if chat != c.wantChat || issue != c.wantIssue {
				t.Fatalf("claimed chat=%d issue=%d, want chat=%d issue=%d", chat, issue, c.wantChat, c.wantIssue)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

// queueChatTask queues a chat task on its own agent and session. When
// chatType is not empty it also freezes a channel delivery of that type, the
// way a Slack or Lark turn does.
func (f *claimDecisionFixture) queueChatTask(t *testing.T, name, chatType string) string {
	t.Helper()
	agentID := f.fx.Agent(t, name, util.UUIDToString(f.runtimeID), testutil.Cols{
		"owner_id":             f.fx.UserID,
		"max_concurrent_tasks": 5,
	})
	session := f.fx.ChatSession(t, agentID)
	taskID := f.fx.Task(t, agentID, testutil.Cols{
		"runtime_id":      util.UUIDToString(f.runtimeID),
		"chat_session_id": session,
		"status":          "queued",
	})
	if chatType != "" {
		f.fx.InsertNoID(t, "channel_task_delivery", testutil.Cols{
			"task_id":         taskID,
			"binding_id":      uuid.NewString(),
			"installation_id": uuid.NewString(),
			"channel_type":    "slack",
			"channel_chat_id": "C-" + name,
			"chat_type":       chatType,
			"route_revision":  1,
		}, "task_id = $1", taskID)
	}
	return taskID
}

// A group channel task is a chat task but not a DM: it must not take a
// reserved slot, and it must count against the non-DM budget.
func TestClaimTasksForRuntimesGroupChannelTaskIsNotDM(t *testing.T) {
	setup := func(t *testing.T) (*claimDecisionFixture, string, string) {
		f := newClaimDecisionFixture(t, 5)
		f.insertTask(t, "queued")
		f.addRuntimeAgentWithTask(t, "reserved-slots-issue-agent-b")
		groupID := f.queueChatTask(t, "reserved-slots-group-agent", "group")
		dmID := f.queueChatTask(t, "reserved-slots-p2p-agent", "p2p")
		return f, groupID, dmID
	}
	claimedIDs := func(tasks []db.AgentTaskQueue) map[string]bool {
		ids := map[string]bool{}
		for _, task := range tasks {
			ids[util.UUIDToString(task.ID)] = true
		}
		return ids
	}

	t.Run("budget spent skips the group task and claims the DM", func(t *testing.T) {
		f, groupID, dmID := setup(t)
		claimed, _, err := f.svc.ClaimTasksForRuntimesWithNonDMLimit(context.Background(), []pgtype.UUID{f.runtimeID}, 4, intPtr(2))
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		ids := claimedIDs(claimed)
		if ids[groupID] || !ids[dmID] || len(claimed) != 3 {
			t.Fatalf("claimed %d tasks, group=%v dm=%v; want 2 issue + the DM and no group task", len(claimed), ids[groupID], ids[dmID])
		}
	})

	t.Run("a claimed group task spends the budget", func(t *testing.T) {
		f, groupID, dmID := setup(t)
		claimed, _, err := f.svc.ClaimTasksForRuntimesWithNonDMLimit(context.Background(), []pgtype.UUID{f.runtimeID}, 4, intPtr(3))
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		ids := claimedIDs(claimed)
		if !ids[groupID] || !ids[dmID] || len(claimed) != 4 {
			t.Fatalf("claimed %d tasks, group=%v dm=%v; want 2 issue + group + DM", len(claimed), ids[groupID], ids[dmID])
		}
	})

	t.Run("no limit equals base", func(t *testing.T) {
		f, groupID, dmID := setup(t)
		claimed, _, err := f.svc.ClaimTasksForRuntimesWithNonDMLimit(context.Background(), []pgtype.UUID{f.runtimeID}, 4, nil)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		ids := claimedIDs(claimed)
		if !ids[groupID] || !ids[dmID] || len(claimed) != 4 {
			t.Fatalf("claimed %d tasks; want all 4 with no limit", len(claimed))
		}
	})
}
