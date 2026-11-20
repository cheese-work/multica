package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// reserved_chat_slots (CHE-8748): the daemon passes an issue budget with each
// batch claim; the server must keep the remaining slots for DM (chat) tasks.

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

func TestClaimTasksForRuntimesIssueLimitKeepsSlotsForChat(t *testing.T) {
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
			claimed, err := f.svc.ClaimTasksForRuntimesWithIssueLimit(context.Background(), []pgtype.UUID{f.runtimeID}, c.maxTasks, c.limit)
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
