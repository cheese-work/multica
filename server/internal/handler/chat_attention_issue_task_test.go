package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6360: an issue task reads the stale DMs of the issue's requester, read-only,
// so the morning brief can run `multica chat attention --stale` live.
type staleDMFixture struct {
	agentID, issueID, taskID, requesterID string
}

func newStaleDMFixture(t *testing.T, requester bool) staleDMFixture {
	t.Helper()
	agentID := createHandlerTestAgent(t, "stale-dm-"+uuid.NewString()[:8], []byte("[]"))
	cols := testutil.Cols{"assignee_type": "agent", "assignee_id": agentID}
	requesterID := dbfx.User(t, "Stale requester", "stale-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": requesterID, "role": "member"})
	if requester {
		cols["requested_by_id"] = requesterID
	}
	issueID := dbfx.Issue(t, "stale dm brief", cols)
	taskID := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "status": "running", "runtime_id": handlerTestRuntimeID(t)})
	return staleDMFixture{agentID, issueID, taskID, requesterID}
}

// dm seeds a DM owned by userID whose open ask is openedAgo old.
func (f staleDMFixture) dm(t *testing.T, userID, title string, openedAgo time.Duration) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
		VALUES ($1, $2, $3, $4, 'active', now()) RETURNING id`, testWorkspaceID, f.agentID, userID, title).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_attention_item WHERE chain_root_session_id = $1`, id)
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, id)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, id)
	})
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO chat_attention_item (workspace_id, chain_root_session_id, chat_session_id, user_id, agent_id, kind, opened_at)
		VALUES ($1, $2, $2, $3, $4, 'ask', $5)`, testWorkspaceID, id, userID, f.agentID, time.Now().Add(-openedAgo)); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO chat_message (chat_session_id, role, content) VALUES ($1, 'assistant', 'SECRET BODY')`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f staleDMFixture) list(t *testing.T, query string) *testutil.Response {
	t.Helper()
	req := withChatTestWorkspaceCtx(t, newRequest(http.MethodGet, "/api/chat/sessions"+query, nil))
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", f.agentID)
	req.Header.Set("X-Task-ID", f.taskID)
	return testutil.Call(t, testHandler.ListChatSessions, req)
}

func TestIssueTaskListsOnlyRequestersStaleDMs(t *testing.T) {
	f := newStaleDMFixture(t, true)
	red := f.dm(t, f.requesterID, "Red DM", 25*time.Hour)
	yellow := f.dm(t, f.requesterID, "Yellow DM", 5*time.Hour)
	fresh := f.dm(t, f.requesterID, "Fresh DM", time.Hour)
	other := f.dm(t, testUserID, "Other person stale DM", 30*time.Hour) // person B

	res := f.list(t, "").Want(http.StatusOK)
	raw := res.Text()
	if strings.Contains(raw, "SECRET BODY") || strings.Contains(raw, other) || strings.Contains(raw, "Other person") {
		t.Fatalf("response leaks message text or another person's DM: %s", raw)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, row := range rows {
		got[row["id"].(string)], _ = row["stale_level"].(string)
		for key := range row {
			switch key {
			case "id", "title", "status", "stale_level", "attention_source", "attention_since", "next_change_at":
			default:
				t.Fatalf("response carries field %q beyond what --stale output uses", key)
			}
		}
	}
	if len(got) != 2 || got[red] != "red" || got[yellow] != "yellow" {
		t.Fatalf("stale list = %v, want only red %s and yellow %s (fresh %s excluded)", got, red, yellow, fresh)
	}
}

func TestIssueTaskWithoutRequesterIsDenied(t *testing.T) {
	f := newStaleDMFixture(t, false)
	f.dm(t, testUserID, "Someone's stale DM", 30*time.Hour)
	if body := f.list(t, "").Want(http.StatusForbidden).Text(); !strings.Contains(body, "no requester") {
		t.Fatalf("denial = %s, want it to name the missing requester", body)
	}
}

func TestIssueTaskRequesterWhoLeftIsDenied(t *testing.T) {
	f := newStaleDMFixture(t, true)
	f.dm(t, f.requesterID, "Red DM", 25*time.Hour)
	dbfx.Exec(t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, f.requesterID)
	f.list(t, "").Want(http.StatusForbidden)
}
