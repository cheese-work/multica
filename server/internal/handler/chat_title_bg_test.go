package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-4106: the DM title is a bg:dm_title job. These tests cover the enqueue,
// the writer, and the fallbacks. None of them may reach server/pkg/llm.

// countLLM points the handler's LLM client at a server that counts requests.
func countLLM(t *testing.T) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"LLM Title"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	prev := testHandler.LLM
	testHandler.LLM = llm.New(llm.Config{APIKey: "test-key", BaseURL: srv.URL})
	t.Cleanup(func() { testHandler.LLM = prev })
	return &hits
}

// dmTitleFixture returns a workspace whose dm_title job points at an online
// agent whose daemon advertises the output-only capability, plus a DM in it.
func dmTitleFixture(t *testing.T, slug, title string) (wsID, runtimeID string, sessionID string) {
	t.Helper()
	wsID, agentID, _, _ := bgFixture(t, slug)
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id=$1`, agentID).Scan(&runtimeID)
	dbfx.Exec(t, `UPDATE agent_runtime SET provider='claude', status='online', last_seen_at=now(), metadata=jsonb_build_object('capabilities', jsonb_build_array($2::text)) WHERE id=$1`,
		runtimeID, protocol.DaemonCapabilityBackgroundOutputOnlyV1)
	putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": agentID}}, nil).Want(http.StatusOK)
	sessionID = dbfx.ChatSession(t, agentID, testutil.Cols{"workspace_id": wsID, "title": title})
	return wsID, runtimeID, sessionID
}

type dmTitleTask struct {
	ID, Status, Input string
}

func dmTitleTasks(t *testing.T, sessionID string) []dmTitleTask {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT id, status, coalesce(context->>'input','') FROM agent_task_queue WHERE source_chat_session_id=$1 AND purpose='bg:dm_title' ORDER BY created_at`, sessionID)
	if err != nil {
		t.Fatalf("query tasks: %v", err)
	}
	defer rows.Close()
	var out []dmTitleTask
	for rows.Next() {
		var x dmTitleTask
		if err := rows.Scan(&x.ID, &x.Status, &x.Input); err != nil {
			t.Fatal(err)
		}
		dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", x.ID)
		out = append(out, x)
	}
	return out
}

func sessionTitle(t *testing.T, sessionID string) string {
	t.Helper()
	return chatSessionTitleFromDB(t, util.MustParseUUID(sessionID))
}

// completeDMTitle finishes the queued task the way the daemon does: the result
// bytes are the JSON-encoded completion request.
func completeDMTitle(t *testing.T, task dmTitleTask, output string) {
	t.Helper()
	result, err := json.Marshal(TaskCompleteRequest{Output: output})
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, task.ID)
	if _, _, err := testHandler.TaskService.CompleteTaskWithRunContext(context.Background(), util.MustParseUUID(task.ID), result, "", "", "", false, "", "", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

func TestDMTitleJobWritesSanitizedTitle(t *testing.T) {
	requireDB(t)
	hits := countLLM(t)
	wsID, _, sid := dmTitleFixture(t, "dmt-happy", "first message")

	testHandler.queueChatTitleJob(wsID, testUserID, util.MustParseUUID(sid), "first message", "first message")

	tasks := dmTitleTasks(t, sid)
	if len(tasks) != 1 || tasks[0].Status != "queued" {
		t.Fatalf("tasks = %+v, want one queued bg:dm_title task", tasks)
	}
	if got := sessionTitle(t, sid); got != "first message" {
		t.Fatalf("title changed before the job ran: %q", got)
	}
	completeDMTitle(t, tasks[0], `"Title: Fix Login Loop."`)
	if got := sessionTitle(t, sid); got != "Fix Login Loop" {
		t.Fatalf("title = %q, want the sanitized job output", got)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("server/pkg/llm received %d requests, want 0", n)
	}
}

func TestDMTitleJobInputIsCapped(t *testing.T) {
	requireDB(t)
	wsID, _, sid := dmTitleFixture(t, "dmt-cap", "x")
	long := strings.Repeat("字", 5000)

	testHandler.queueChatTitleJob(wsID, testUserID, util.MustParseUUID(sid), "x", long)

	tasks := dmTitleTasks(t, sid)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one", tasks)
	}
	in := tasks[0].Input
	if n := len([]rune(in)); n != chatTitleJobInputMaxRunes {
		t.Fatalf("queued input runes = %d, want the full input capped at %d", n, chatTitleJobInputMaxRunes)
	}
	if !strings.HasPrefix(in, chatTitleSystemPrompt) || !strings.Contains(in, "字") {
		t.Fatalf("capped input lost the prompt or the message: %.80q", in)
	}
}

// A rename during the run wins: the writer's CAS finds a different title.
func TestDMTitleJobRenameDuringRunWins(t *testing.T) {
	requireDB(t)
	wsID, _, sid := dmTitleFixture(t, "dmt-rename", "first message")
	testHandler.queueChatTitleJob(wsID, testUserID, util.MustParseUUID(sid), "first message", "first message")
	tasks := dmTitleTasks(t, sid)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one", tasks)
	}

	if _, err := testHandler.Queries.UpdateChatSessionTitle(context.Background(), db.UpdateChatSessionTitleParams{
		ID: util.MustParseUUID(sid), Title: "My Renamed Chat",
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	completeDMTitle(t, tasks[0], "Generated Title")

	if got := sessionTitle(t, sid); got != "My Renamed Chat" {
		t.Fatalf("title = %q, want the rename to win", got)
	}
}

// A rename back to the observed title (A to B to A) still wins: a manual rename
// marks the title as set by hand, so the writer's CAS never lands.
func TestDMTitleJobRenameBackToOriginalWins(t *testing.T) {
	requireDB(t)
	wsID, _, sid := dmTitleFixture(t, "dmt-aba", "Original")
	testHandler.queueChatTitleJob(wsID, testUserID, util.MustParseUUID(sid), "Original", "Opening message")
	tasks := dmTitleTasks(t, sid)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one", tasks)
	}
	for _, title := range []string{"Temporary", "Original"} {
		if _, err := testHandler.Queries.UpdateChatSessionTitle(context.Background(), db.UpdateChatSessionTitleParams{
			ID: util.MustParseUUID(sid), Title: title,
		}); err != nil {
			t.Fatalf("rename: %v", err)
		}
	}
	completeDMTitle(t, tasks[0], "Generated Title")
	if got := sessionTitle(t, sid); got != "Original" {
		t.Fatalf("title = %q, want the manual rename kept", got)
	}
}

// The writer rejects output that sanitizes to nothing and keeps the title.
func TestDMTitleJobUnusableOutputKeepsTitle(t *testing.T) {
	requireDB(t)
	wsID, _, sid := dmTitleFixture(t, "dmt-empty", "first message")
	testHandler.queueChatTitleJob(wsID, testUserID, util.MustParseUUID(sid), "first message", "first message")
	tasks := dmTitleTasks(t, sid)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one", tasks)
	}
	completeDMTitle(t, tasks[0], `"。"`)
	if got := sessionTitle(t, sid); got != "first message" {
		t.Fatalf("title = %q, want unchanged", got)
	}
}

// No reachable agent: nothing is queued, the deterministic title stays, and
// the LLM client is never called.
func TestDMTitleJobNoAgentKeepsFallbackTitle(t *testing.T) {
	requireDB(t)
	fallback := strings.Repeat("a", 80)
	for _, c := range []struct {
		name  string
		setup func(wsID, runtimeID string)
	}{
		{"daemon offline", func(_, rt string) { dbfx.Exec(t, `UPDATE agent_runtime SET status='offline' WHERE id=$1`, rt) }},
		{"daemon lacks capability", func(_, rt string) {
			dbfx.Exec(t, `UPDATE agent_runtime SET metadata='{}'::jsonb WHERE id=$1`, rt)
		}},
		{"no dm_title configured", func(ws, _ string) {
			dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'background_agents' WHERE id=$1`, ws)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			hits := countLLM(t)
			wsID, rt, sid := dmTitleFixture(t, "dmt-none-"+strings.ReplaceAll(c.name, " ", "-"), fallback)
			c.setup(wsID, rt)

			testHandler.queueChatTitleJob(wsID, testUserID, util.MustParseUUID(sid), fallback, fallback+" and more text")

			if tasks := dmTitleTasks(t, sid); len(tasks) != 0 {
				t.Fatalf("tasks = %+v, want none", tasks)
			}
			if got := sessionTitle(t, sid); got != fallback {
				t.Fatalf("title = %q, want the 60-char fallback", got)
			}
			if n := hits.Load(); n != 0 {
				t.Fatalf("server/pkg/llm received %d requests, want 0", n)
			}
		})
	}
}

// The channel hook queues the same job from the current turn and, when the job
// completes, publishes the title on the chat:session_updated channel.
func TestChannelChatTitleQueuesJobAndPublishesTitle(t *testing.T) {
	requireDB(t)
	hits := countLLM(t)
	wsID, _, sid := dmTitleFixture(t, "dmt-channel", "Investigate the login redirect")
	updates := make(chan protocol.ChatSessionUpdatedPayload, 1)
	testHandler.Bus.Subscribe(protocol.EventChatSessionUpdated, func(e events.Event) {
		if p, ok := e.Payload.(protocol.ChatSessionUpdatedPayload); ok && p.ChatSessionID == sid {
			select {
			case updates <- p:
			default:
			}
		}
	})

	const current = "Investigate the login redirect"
	testHandler.GenerateChannelChatTitle(util.MustParseUUID(wsID), util.MustParseUUID(testUserID), util.MustParseUUID(sid), current, current)

	// The hook queues off the caller's path; wait for the task row.
	var tasks []dmTitleTask
	for deadline := time.Now().Add(5 * time.Second); len(tasks) == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		tasks = dmTitleTasks(t, sid)
	}
	if len(tasks) != 1 || !strings.HasSuffix(tasks[0].Input, "\n"+current) {
		t.Fatalf("tasks = %+v, want one whose input ends with the current turn", tasks)
	}
	completeDMTitle(t, tasks[0], "Login redirect investigation")
	select {
	case p := <-updates:
		if p.Title != "Login redirect investigation" {
			t.Fatalf("title update = %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no chat:session_updated event after the job completed")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("server/pkg/llm received %d requests, want 0", n)
	}
}
