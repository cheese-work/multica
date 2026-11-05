package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Quick actions as a bg:quick_actions background job (CHE-4108, CHE-4013 step
// 13). The fixture is a DM with one finished chat turn whose assistant reply is
// the message the pills attach to.

type quickActionsFixture struct {
	*bgFailoverFixture
	turn db.AgentTaskQueue // the finished chat turn the pills belong to
}

func newQuickActionsFixture(t *testing.T) *quickActionsFixture {
	t.Helper()
	b := newBGFailoverFixture(t)
	turnID := b.rows.Task(t, b.primary, dbfx.Cols{"status": "completed", "chat_session_id": b.dm, "runtime_id": b.primaryRuntime})
	b.rows.Insert(t, "chat_message", dbfx.Cols{
		"chat_session_id": b.dm, "role": "assistant", "task_id": turnID, "content": "I can refactor parser.go or add tests.",
	})
	turn, err := b.svc.Queries.GetAgentTask(context.Background(), util.MustParseUUID(turnID))
	if err != nil {
		t.Fatal(err)
	}
	return &quickActionsFixture{bgFailoverFixture: b, turn: turn}
}

// configure sets the workspace's quick_actions chain: primary, then fallback.
func (q *quickActionsFixture) configure(t *testing.T) {
	t.Helper()
	q.rows.Exec(t, `UPDATE workspace SET settings = jsonb_build_object('background_agents', jsonb_build_object(
		'quick_actions', jsonb_build_object('agent_id', $2::text, 'fallback_agent_id', $3::text))) WHERE id = $1`,
		q.workspaceID, q.primary, q.fallback)
}

func (q *quickActionsFixture) tasks(t *testing.T) int {
	t.Helper()
	return q.rows.Count(t, `SELECT count(*) FROM agent_task_queue WHERE purpose = 'bg:quick_actions' AND status IN ('queued','dispatched','running')`)
}

func (q *quickActionsFixture) pills(t *testing.T) string {
	t.Helper()
	var raw []byte
	if err := q.pool.QueryRow(context.Background(), `SELECT quick_actions FROM chat_message WHERE task_id = $1`, util.UUIDToString(q.turn.ID)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (q *quickActionsFixture) complete(t *testing.T, output string) {
	t.Helper()
	var id string
	if err := q.pool.QueryRow(context.Background(), `SELECT id::text FROM agent_task_queue WHERE purpose = 'bg:quick_actions' AND status = 'queued'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	q.rows.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, id)
	result, _ := json.Marshal(map[string]string{"output": output})
	if _, err := q.svc.CompleteTask(context.Background(), util.MustParseUUID(id), result, "", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestQuickActionsNoAgentMeansNoTaskAndNoPills(t *testing.T) {
	q := newQuickActionsFixture(t)
	msg, err := q.svc.Queries.GetChatMessageByTaskAssistant(context.Background(), q.turn.ID)
	if err != nil {
		t.Fatal(err)
	}

	if q.svc.chatQuickActionsEligible(context.Background(), q.turn, &msg) {
		t.Fatal("a turn with no quick_actions agent is eligible: clients would wait on pills that never come")
	}
	q.svc.GenerateChatQuickActionsAsync(q.turn, ChatQuickActionsAutomatic)
	if got := q.tasks(t); got != 0 {
		t.Fatalf("no agent configured but %d task(s) queued", got)
	}

	// Control: the same turn with a chain configured is eligible and queues one task.
	q.configure(t)
	if !q.svc.chatQuickActionsEligible(context.Background(), q.turn, &msg) {
		t.Fatal("a turn with a quick_actions agent is not eligible")
	}
	q.svc.GenerateChatQuickActionsAsync(q.turn, ChatQuickActionsAutomatic)
	if got := q.tasks(t); got != 1 {
		t.Fatalf("configured chain queued %d task(s), want 1", got)
	}
}

func TestQuickActionsDropsAtTheConcurrencyCeiling(t *testing.T) {
	q := newQuickActionsFixture(t)
	q.configure(t)
	other := make([]string, 0, chatQuickActionsMaxConcurrent)
	for range chatQuickActionsMaxConcurrent {
		other = append(other, q.rows.Task(t, q.primary, dbfx.Cols{"purpose": "bg:quick_actions", "status": "queued", "runtime_id": q.primaryRuntime}))
	}

	q.svc.GenerateChatQuickActionsAsync(q.turn, ChatQuickActionsAutomatic)
	if got := q.tasks(t); got != chatQuickActionsMaxConcurrent {
		t.Fatalf("at the ceiling %d tasks are active, want %d (the pass must be dropped, not queued)", got, chatQuickActionsMaxConcurrent)
	}

	// Control: one slot frees up, the next pass is admitted.
	q.rows.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, other[0])
	q.svc.GenerateChatQuickActionsAsync(q.turn, ChatQuickActionsAutomatic)
	if got := q.tasks(t); got != chatQuickActionsMaxConcurrent {
		t.Fatalf("after a slot freed %d tasks are active, want %d", got, chatQuickActionsMaxConcurrent)
	}
	if got := q.rows.Count(t, `SELECT count(*) FROM agent_task_queue WHERE purpose = 'bg:quick_actions' AND source_chat_session_id = $1`, q.dm); got != 1 {
		t.Fatalf("tasks for this DM = %d, want 1 (admitted once the ceiling had room)", got)
	}
}

func TestQuickActionsOnePassPerSession(t *testing.T) {
	q := newQuickActionsFixture(t)
	q.configure(t)
	q.svc.GenerateChatQuickActionsAsync(q.turn, ChatQuickActionsAutomatic)
	q.svc.GenerateChatQuickActionsAsync(q.turn, ChatQuickActionsRefresh)
	if got := q.tasks(t); got != 1 {
		t.Fatalf("two passes for one DM queued %d task(s), want 1", got)
	}
}

// newTurn adds a second DM with its own finished chat turn.
func (q *quickActionsFixture) newTurn(t *testing.T) db.AgentTaskQueue {
	t.Helper()
	dm := q.rows.ChatSession(t, q.primary)
	id := q.rows.Task(t, q.primary, dbfx.Cols{"status": "completed", "chat_session_id": dm, "runtime_id": q.primaryRuntime})
	q.rows.Insert(t, "chat_message", dbfx.Cols{"chat_session_id": dm, "role": "assistant", "task_id": id, "content": "I can refactor parser.go."})
	turn, err := q.svc.Queries.GetAgentTask(context.Background(), util.MustParseUUID(id))
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

// fire runs one pass per turn at the same moment.
func (q *quickActionsFixture) fire(turns []db.AgentTaskQueue) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, turn := range turns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			q.svc.GenerateChatQuickActionsAsync(turn, ChatQuickActionsAutomatic)
		}()
	}
	close(start)
	wg.Wait()
}

func TestQuickActionsConcurrentPassesRespectTheCeiling(t *testing.T) {
	q := newQuickActionsFixture(t)
	q.configure(t)
	for range chatQuickActionsMaxConcurrent - 1 {
		q.rows.Task(t, q.primary, dbfx.Cols{"purpose": "bg:quick_actions", "status": "queued", "runtime_id": q.primaryRuntime})
	}
	turns := []db.AgentTaskQueue{q.turn}
	for range 7 {
		turns = append(turns, q.newTurn(t))
	}

	q.fire(turns)
	if got := q.tasks(t); got != chatQuickActionsMaxConcurrent {
		t.Fatalf("8 racing passes with one free slot left %d active tasks, want exactly %d", got, chatQuickActionsMaxConcurrent)
	}
}

func TestQuickActionsConcurrentPassesForOneSessionQueueOnce(t *testing.T) {
	q := newQuickActionsFixture(t)
	q.configure(t)
	q.fire([]db.AgentTaskQueue{q.turn, q.turn, q.turn, q.turn, q.turn, q.turn, q.turn, q.turn})
	if got := q.tasks(t); got != 1 {
		t.Fatalf("8 racing passes for one DM queued %d task(s), want 1", got)
	}
}

func TestQuickActionsGoodOutputIsSanitisedAndWritten(t *testing.T) {
	q := newQuickActionsFixture(t)
	q.configure(t)
	q.svc.GenerateChatQuickActionsAsync(q.turn, ChatQuickActionsAutomatic)
	q.complete(t, `{"actions":[{"label":"  Add   tests ","prompt":""},{"label":"add tests"},{"label":"Refactor","prompt":"Refactor parser.go","primary":true}]}`)

	var got []protocol.ChatQuickAction
	if err := json.Unmarshal([]byte(q.pills(t)), &got); err != nil {
		t.Fatalf("pills %q: %v", q.pills(t), err)
	}
	if len(got) != 2 || got[0].Label != "Add tests" || got[0].Prompt != "Add tests" || got[0].Primary || !got[1].Primary {
		t.Fatalf("pills = %+v, want two sanitised actions with the primary on Refactor", got)
	}
}

func TestQuickActionsBadOutputWritesNothingAndDoesNotFailOver(t *testing.T) {
	q := newQuickActionsFixture(t)
	q.configure(t)
	q.svc.GenerateChatQuickActionsAsync(q.turn, ChatQuickActionsAutomatic)
	if got := q.tasks(t); got != 1 {
		t.Fatalf("queued %d task(s), want 1", got)
	}
	q.complete(t, "sorry, I cannot help with that")

	if got := q.pills(t); got != "[]" && got != "" && got != "null" {
		t.Fatalf("bad output wrote pills: %s", got)
	}
	if got := q.queued(t, q.fallback) + q.rows.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1 AND purpose = 'bg:quick_actions'`, q.fallback); got != 0 {
		t.Fatalf("bad output failed over: %d fallback task(s)", got)
	}
}

// The pass runs after the turn lands, so a newer user message may already be in
// the session. The prompt must end at the target reply and the pills must land
// on the target turn.
func TestQuickActionsContextAnchorsOnTargetTurn(t *testing.T) {
	q := newQuickActionsFixture(t)
	q.configure(t)
	q.rows.Insert(t, "chat_message", dbfx.Cols{"chat_session_id": q.dm, "role": "user", "content": "NEWER USER MESSAGE"})

	q.svc.GenerateChatQuickActionsAsync(q.turn, ChatQuickActionsAutomatic)

	var input string
	if err := q.pool.QueryRow(context.Background(),
		`SELECT context->>'input' FROM agent_task_queue WHERE purpose = 'bg:quick_actions' AND status = 'queued'`).Scan(&input); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input, "I can refactor parser.go") {
		t.Fatalf("prompt must end at the target reply, got:\n%s", input)
	}
	if strings.Contains(input, "NEWER USER MESSAGE") {
		t.Fatalf("prompt must exclude turns newer than the target, got:\n%s", input)
	}
	q.complete(t, `{"actions":[{"label":"Next","prompt":"do the next thing","primary":true}]}`)
	if got := q.pills(t); !strings.Contains(got, "Next") {
		t.Fatalf("target turn pills = %s", got)
	}
}

// The admission lock holds one connection. Every read and the insert must run
// on that connection: with more concurrent admissions than pool connections, a
// path that goes back to the pool deadlocks until the enqueue deadline.
func TestQuickActionsAdmissionDoesNotStarveASmallPool(t *testing.T) {
	q := newQuickActionsFixture(t)
	q.configure(t)
	cfg := q.pool.Config().Copy()
	cfg.MaxConns = 2
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(small.Close)
	svc := NewTaskService(db.New(small), small, nil, events.New())

	turns := []db.AgentTaskQueue{q.turn}
	for range 5 {
		turns = append(turns, q.newTurn(t))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	began := time.Now()
	for _, turn := range turns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			svc.GenerateChatQuickActionsAsync(turn, ChatQuickActionsAutomatic)
		}()
	}
	close(start)
	wg.Wait()

	if took := time.Since(began); took > chatQuickActionsEnqueueTimeout/2 {
		t.Fatalf("%d admissions on a 2-connection pool took %s: the lock transaction starved the pool", len(turns), took)
	}
	if got := q.tasks(t); got != len(turns) {
		t.Fatalf("%d admissions on a 2-connection pool queued %d tasks, want %d", len(turns), got, len(turns))
	}
}
