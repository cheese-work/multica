package service

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type interimStep struct{ typ, content string }

// interimFixture seeds a running direct-chat task with a user input.
type interimFixture struct {
	ctx           context.Context
	pool          *pgxpool.Pool
	q             *db.Queries
	svc           *TaskService
	task          db.AgentTaskQueue
	taskID        string
	chatSessionID string
	nextSeq       int
}

func newInterimFixture(t *testing.T) *interimFixture {
	t.Helper()
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	_ = workspaceID

	var chatSessionID, taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id)
		VALUES ($1, $2, $3) RETURNING id`, workspaceID, agentID, userID).Scan(&chatSessionID); err != nil {
		t.Fatalf("seed chat session: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority)
		VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), $2, 'running', 2) RETURNING id::text`,
		agentID, chatSessionID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM task_message WHERE task_id = $1`, taskID)
		pool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, chatSessionID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
		pool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, chatSessionID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content, task_id)
		VALUES ($1, 'user', 'do the thing', $2)`, chatSessionID, taskID); err != nil {
		t.Fatalf("seed input: %v", err)
	}
	return &interimFixture{
		ctx: ctx, pool: pool, q: q,
		svc:           &TaskService{Queries: q, TxStarter: pool, Bus: events.New()},
		taskID:        taskID,
		chatSessionID: chatSessionID,
		task: db.AgentTaskQueue{
			ID:              util.MustParseUUID(taskID),
			ChatSessionID:   util.MustParseUUID(chatSessionID),
			ChatInputTaskID: util.MustParseUUID(taskID),
			AgentID:         util.MustParseUUID(agentID),
		},
	}
}

func (f *interimFixture) addSteps(t *testing.T, steps ...interimStep) {
	t.Helper()
	for _, m := range steps {
		f.nextSeq++
		if _, err := f.pool.Exec(f.ctx, `
			INSERT INTO task_message (task_id, seq, type, content) VALUES ($1, $2, $3, NULLIF($4, ''))`,
			f.taskID, f.nextSeq, m.typ, m.content); err != nil {
			t.Fatalf("seed task_message %d: %v", f.nextSeq, err)
		}
	}
}

func (f *interimFixture) complete(t *testing.T, output string) {
	t.Helper()
	result, _ := json.Marshal(protocol.TaskCompletedPayload{Output: output})
	if _, err := f.svc.writeChatCompletionOutcome(f.ctx, f.q, f.task, result); err != nil {
		t.Fatalf("writeChatCompletionOutcome: %v", err)
	}
}

func (f *interimFixture) assistantRows(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, `
		SELECT content FROM chat_message
		WHERE chat_session_id = $1 AND role = 'assistant' ORDER BY created_at, id`, f.chatSessionID)
	if err != nil {
		t.Fatalf("list assistant rows: %v", err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, c)
	}
	return got
}

func (f *interimFixture) wantRows(t *testing.T, want ...string) {
	t.Helper()
	if got := f.assistantRows(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("assistant rows = %q, want %q", got, want)
	}
}

// TestWriteChatCompletionOutcomeKeepsInterimTextVisible pins CHE-4704: a text
// block the agent wrote BEFORE its last tool step is a message for the human,
// not step-block noise. Only the final Output used to become a chat_message, so
// the earlier lines stayed in task_message and folded into the step block.
func TestWriteChatCompletionOutcomeKeepsInterimTextVisible(t *testing.T) {
	f := newInterimFixture(t)
	// 2 interim lines (each followed by a tool step), thinking, then the final text.
	f.addSteps(t,
		interimStep{"thinking", "hmm"},
		interimStep{"text", "Creating the issue…"},
		interimStep{"tool_use", ""},
		interimStep{"tool_result", ""},
		interimStep{"text", "Reading the log…"},
		interimStep{"tool_use", ""},
		interimStep{"tool_result", ""},
		interimStep{"text", "All done."},
	)
	f.complete(t, "All done.")
	f.wantRows(t, "Creating the issue…", "Reading the log…", "All done.")
}

// A provider that reports only its last message as Output (Codex) leaves
// earlier text after the last tool step in the transcript; it is still a
// message for the human (CHE-4704 review P1).
func TestWriteChatCompletionOutcomeKeepsTextBeforeFinalAfterLastTool(t *testing.T) {
	f := newInterimFixture(t)
	f.addSteps(t,
		interimStep{"tool_use", ""},
		interimStep{"tool_result", ""},
		interimStep{"text", "Checked the log."},
		interimStep{"text", "All done."},
	)
	f.complete(t, "All done.")
	f.wantRows(t, "Checked the log.", "All done.")
}

// A provider can end its stream with text that is not part of the terminal
// result (Claude: streamed text, then a separate result). The unmatched
// trailing text is still a message for the human (CHE-4704 re-review P1).
func TestWriteChatCompletionOutcomeKeepsTrailingTextWhenOutputDiffers(t *testing.T) {
	f := newInterimFixture(t)
	f.addSteps(t,
		interimStep{"tool_use", ""},
		interimStep{"tool_result", ""},
		interimStep{"text", "Checking the result"},
	)
	f.complete(t, "Done")
	f.wantRows(t, "Checking the result", "Done")
}

// The Output can match an earlier block while a later, still unsealed block
// holds more text; that trailing text is still a message (CHE-4704 third review P1).
func TestWriteChatCompletionOutcomeKeepsTrailingTextAfterEarlierMatch(t *testing.T) {
	f := newInterimFixture(t)
	f.addSteps(t,
		interimStep{"text", "Done"},
		interimStep{"tool_use", ""},
		interimStep{"tool_result", ""},
		interimStep{"text", "Checking the result"},
	)
	f.complete(t, "Done")
	f.wantRows(t, "Checking the result", "Done")
}

// The daemon seals a text row every 500 ms, mid-sentence. Contiguous text rows
// are one message, whitespace-only chunks included (CHE-4704 review P1).
func TestWriteChatCompletionOutcomeJoinsStreamedTextChunks(t *testing.T) {
	f := newInterimFixture(t)
	f.addSteps(t,
		interimStep{"text", "Creating"},
		interimStep{"text", " "},
		interimStep{"text", "the issue…"},
		interimStep{"tool_use", ""},
		interimStep{"tool_result", ""},
		interimStep{"text", "All "},
		interimStep{"text", "done."},
	)
	f.complete(t, "All done.")
	f.wantRows(t, "Creating the issue…", "All done.")
}

// A reply with no tool step at all is only the final reply.
func TestWriteChatCompletionOutcomeNoToolWritesOnlyFinalReply(t *testing.T) {
	f := newInterimFixture(t)
	f.addSteps(t, interimStep{"text", "Hello"}, interimStep{"text", " there."})
	f.complete(t, "Hello there.")
	f.wantRows(t, "Hello there.")
}

// The interim line must reach the human while the turn still runs, and the
// completion must not write it a second time (CHE-4704 review P1).
func TestPublishChatInterimMessagesShowsSealedTextWhileTurnRuns(t *testing.T) {
	f := newInterimFixture(t)
	var published []protocol.ChatMessagePayload
	f.svc.Bus.Subscribe(protocol.EventChatMessage, func(e events.Event) {
		published = append(published, e.Payload.(protocol.ChatMessagePayload))
	})
	f.addSteps(t,
		interimStep{"text", "Creating"},
		interimStep{"text", " the issue…"},
		interimStep{"tool_use", ""},
		interimStep{"text", "Still typing"}, // unsealed: may be the final reply
	)
	if err := f.svc.PublishChatInterimMessages(f.ctx, f.task); err != nil {
		t.Fatalf("PublishChatInterimMessages: %v", err)
	}
	f.wantRows(t, "Creating the issue…")
	// The row must also reach open clients, without a task_id: with one, web and
	// mobile key it like the final reply and render the whole task timeline.
	if len(published) != 1 || published[0].Content != "Creating the issue…" || published[0].TaskID != "" {
		t.Fatalf("published = %+v, want one chat:message with no task id", published)
	}

	if err := f.svc.PublishChatInterimMessages(f.ctx, f.task); err != nil {
		t.Fatalf("second PublishChatInterimMessages: %v", err)
	}
	f.wantRows(t, "Creating the issue…")
	if len(published) != 1 {
		t.Fatalf("second publish re-announced the row: %+v", published)
	}

	f.addSteps(t, interimStep{"text", " now."})
	f.complete(t, "Still typing now.")
	f.wantRows(t, "Creating the issue…", "Still typing now.")
}
