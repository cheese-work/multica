package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/multica-ai/multica/server/internal/bgagents"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Provider failover and reachability for background tasks (CHE-4102, CHE-4013
// step 7). The fixture has a primary agent on one runtime and a fallback agent
// on another; both runtimes start online with a fresh heartbeat and the
// output_only capability.

type bgFailoverFixture struct {
	*delegatedFailureFixture
	svc                       *TaskService
	rows                      *dbfx.Fixture
	primary, fallback         string
	primaryRuntime, fbRuntime string
	noAgentCalls              []string // slot of each task that ended the chain
}

func newBGFailoverFixture(t *testing.T) *bgFailoverFixture {
	t.Helper()
	f, svc := seedDelegatedFailureFixture(t)
	rows := dbfx.New(f.pool, f.workspaceID, f.userID)
	caps := `jsonb_build_object('capabilities', jsonb_build_array('` + protocol.DaemonCapabilityBackgroundOutputOnlyV1 + `'))`
	runtime := func(name string) string {
		var id string
		if err := f.pool.QueryRow(context.Background(), `
			INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, last_seen_at, metadata)
			VALUES ($1, $2, 'local', 'claude', 'online', now(), `+caps+`) RETURNING id`, f.workspaceID, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	b := &bgFailoverFixture{delegatedFailureFixture: f, svc: svc, rows: rows,
		primary: f.coordinator, fallback: f.worker, primaryRuntime: runtime("bg-primary"), fbRuntime: runtime("bg-fallback")}
	rows.Exec(t, `UPDATE agent SET runtime_id = $2, visibility = 'workspace' WHERE id = $1`, b.primary, b.primaryRuntime)
	rows.Exec(t, `UPDATE agent SET runtime_id = $2, visibility = 'workspace' WHERE id = $1`, b.fallback, b.fbRuntime)
	svc.BackgroundNoAgent = map[string]BackgroundNoAgentPath{
		bgagents.Purpose(bgagents.DMTitle): func(_ context.Context, task db.AgentTaskQueue) {
			var c BackgroundTaskContext
			_ = json.Unmarshal(task.Context, &c)
			b.noAgentCalls = append(b.noAgentCalls, c.Slot)
		},
	}
	return b
}

func (b *bgFailoverFixture) input() BackgroundTaskInput {
	return BackgroundTaskInput{
		Job: bgagents.DMTitle, WorkspaceID: util.MustParseUUID(b.workspaceID), MemberID: util.MustParseUUID(b.userID),
		Source: bgagents.UserSource, Input: "hello",
	}
}

func (b *bgFailoverFixture) chain() []bgagents.Candidate {
	return []bgagents.Candidate{{Slot: bgagents.Primary, AgentID: b.primary}, {Slot: bgagents.Fallback, AgentID: b.fallback}}
}

func (b *bgFailoverFixture) enqueue(t *testing.T) db.AgentTaskQueue {
	t.Helper()
	task, err := b.svc.EnqueueBackgroundChain(context.Background(), b.input(), b.chain())
	if err != nil {
		t.Fatalf("enqueue chain: %v", err)
	}
	return task
}

func (b *bgFailoverFixture) fail(t *testing.T, task db.AgentTaskQueue, reason string) {
	t.Helper()
	b.rows.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, util.UUIDToString(task.ID))
	if _, err := b.svc.FailTask(context.Background(), task.ID, "provider returned 500", "", "", "", reason, false, "", ""); err != nil {
		t.Fatal(err)
	}
}

func (b *bgFailoverFixture) queued(t *testing.T, agent string) int {
	t.Helper()
	return b.rows.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1 AND purpose = 'bg:dm_title' AND status = 'queued'`, agent)
}

func TestBackgroundFailoverRunsTheFallbackOnceOnProviderFailure(t *testing.T) {
	b := newBGFailoverFixture(t)
	primary := b.enqueue(t)
	b.fail(t, primary, providerDown)
	if got := b.queued(t, b.fallback); got != 1 {
		t.Fatalf("fallback runs queued = %d, want 1", got)
	}
	var slot, input, member string
	if err := b.pool.QueryRow(context.Background(), `SELECT context->>'slot', context->>'input', context->>'member_id'
		FROM agent_task_queue WHERE agent_id = $1 AND purpose = 'bg:dm_title'`, b.fallback).Scan(&slot, &input, &member); err != nil {
		t.Fatal(err)
	}
	if slot != "fallback" || input != "hello" || member != b.userID {
		t.Fatalf("fallback task slot/input/member = %q/%q/%q", slot, input, member)
	}
	if len(b.noAgentCalls) != 0 {
		t.Fatalf("no-agent path ran with a fallback available: %v", b.noAgentCalls)
	}
}

func TestBackgroundFailoverEndsTheChainWhenTheFallbackFails(t *testing.T) {
	b := newBGFailoverFixture(t)
	b.fail(t, b.enqueue(t), providerDown)
	var fb db.AgentTaskQueue
	row := b.pool.QueryRow(context.Background(), `SELECT id FROM agent_task_queue WHERE agent_id = $1 AND purpose = 'bg:dm_title'`, b.fallback)
	if err := row.Scan(&fb.ID); err != nil {
		t.Fatal(err)
	}
	b.fail(t, fb, providerDown)
	for _, agent := range []string{b.primary, b.fallback} {
		if got := b.queued(t, agent); got != 0 {
			t.Fatalf("agent %s has %d queued tasks after the chain ended", agent, got)
		}
	}
	if len(b.noAgentCalls) != 1 || b.noAgentCalls[0] != "fallback" {
		t.Fatalf("no-agent path calls = %v, want one for the fallback slot", b.noAgentCalls)
	}
}

func TestBackgroundFailoverSkipsAnUnreachablePrimaryAtEnqueue(t *testing.T) {
	cases := []struct {
		name   string
		break_ string
	}{
		{"stale heartbeat", `UPDATE agent_runtime SET last_seen_at = now() - interval '10 minutes' WHERE id = $1`},
		{"runtime offline", `UPDATE agent_runtime SET status = 'offline' WHERE id = $1`},
		{"daemon without the capability", `UPDATE agent_runtime SET metadata = '{}'::jsonb WHERE id = $1`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBGFailoverFixture(t)
			b.rows.Exec(t, c.break_, b.primaryRuntime)
			task := b.enqueue(t)
			if util.UUIDToString(task.AgentID) != b.fallback {
				t.Fatalf("task went to %s, want the fallback agent", util.UUIDToString(task.AgentID))
			}
			if got := b.queued(t, b.primary); got != 0 {
				t.Fatalf("a task is queued behind the unreachable primary: %d", got)
			}
		})
	}
}

func TestBackgroundEnqueueFindsNoAgentWhenEverySlotIsUnreachable(t *testing.T) {
	b := newBGFailoverFixture(t)
	b.rows.Exec(t, `UPDATE agent_runtime SET status = 'offline' WHERE id = ANY($1::uuid[])`, []string{b.primaryRuntime, b.fbRuntime})
	_, err := b.svc.EnqueueBackgroundChain(context.Background(), b.input(), b.chain())
	if !errors.Is(err, ErrNoReachableBackgroundAgent) {
		t.Fatalf("err = %v, want ErrNoReachableBackgroundAgent", err)
	}
}

func TestBackgroundFailoverSkipsAFallbackThatWentOffline(t *testing.T) {
	b := newBGFailoverFixture(t)
	primary := b.enqueue(t)
	b.rows.Exec(t, `UPDATE agent_runtime SET status = 'offline' WHERE id = $1`, b.fbRuntime)
	b.fail(t, primary, providerDown)
	if got := b.queued(t, b.fallback); got != 0 {
		t.Fatalf("a task is queued behind the offline fallback: %d", got)
	}
	if len(b.noAgentCalls) != 1 || b.noAgentCalls[0] != "primary" {
		t.Fatalf("no-agent path calls = %v, want one for the primary slot", b.noAgentCalls)
	}
}

func TestBackgroundFailoverIgnoresFailuresThatAreNotTheProvider(t *testing.T) {
	b := newBGFailoverFixture(t)
	b.fail(t, b.enqueue(t), "agent_error.process_failure")
	if got := b.queued(t, b.fallback); got != 0 {
		t.Fatalf("a non-provider failure failed over: %d fallback runs", got)
	}
}

func TestBackgroundBadOutputDoesNotFailOver(t *testing.T) {
	b := newBGFailoverFixture(t)
	b.svc.BackgroundWriters = map[string]BackgroundWriter{
		bgagents.Purpose(bgagents.DMTitle): func(context.Context, *db.Queries, db.AgentTaskQueue, []byte) error {
			return ErrInvalidBackgroundOutput
		},
	}
	task := b.enqueue(t)
	b.rows.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, util.UUIDToString(task.ID))
	if _, err := b.svc.CompleteTask(context.Background(), task.ID, []byte(`"   "`), "", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := b.queued(t, b.fallback); got != 0 {
		t.Fatalf("bad output failed over: %d fallback runs", got)
	}
}
