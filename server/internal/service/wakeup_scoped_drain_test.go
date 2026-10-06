package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Scheduler drain of the scoped-event outbox (CHE-1082 L9): bounded batches,
// SKIP LOCKED, one issue per event, the issue's runtime instance ensured and the
// existing wakeup receipts written atomically with marking the input handled.

type drainKit struct {
	scopedKit
	s *IssueWakeupService
}

func newDrainKit(t *testing.T) drainKit {
	t.Helper()
	k := newScopedKit(t)
	k.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", k.issue)
	return drainKit{scopedKit: k, s: &IssueWakeupService{Tasks: k.f.svc.TaskSvc}}
}

func (k drainKit) drain(t *testing.T) {
	t.Helper()
	if err := k.s.DrainScopedEvents(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func (k drainKit) outcomes(t *testing.T) []string {
	t.Helper()
	rows, err := k.f.Pool.Query(context.Background(), `SELECT COALESCE(outcome,'pending') FROM wakeup_scoped_event WHERE workspace_id=$1 ORDER BY captured_at,id`, k.f.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

func (k drainKit) wantOutcomes(t *testing.T, want ...string) {
	t.Helper()
	if got := k.outcomes(t); !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
}

type scopedInstance struct {
	ID, Rule, ScopeKind, ScopeID, Instruction, Agent, CreatedBy, Mode string
	Enabled                                                           bool
	Revision                                                          int64
	Events                                                            []string
	Fingerprint, CapacityReason                                       *string
}

func (k drainKit) instances(t *testing.T, issue string) []scopedInstance {
	t.Helper()
	rows, err := k.f.Pool.Query(context.Background(), `SELECT id::text,default_rule_key,default_scope_kind,default_scope_id::text,instruction,agent_id::text,created_by::text,mode,enabled,revision,event_types,config_fingerprint,capacity_reason
 FROM issue_wakeup WHERE issue_id=$1 AND default_rule_key IS NOT NULL ORDER BY default_rule_key,id`, issue)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []scopedInstance
	for rows.Next() {
		var i scopedInstance
		if err := rows.Scan(&i.ID, &i.Rule, &i.ScopeKind, &i.ScopeID, &i.Instruction, &i.Agent, &i.CreatedBy, &i.Mode, &i.Enabled, &i.Revision, &i.Events, &i.Fingerprint, &i.CapacityReason); err != nil {
			t.Fatal(err)
		}
		out = append(out, i)
	}
	return out
}

type scopedReceipt struct {
	Type      string
	Revision  int64
	Count     int
	Processed bool
}

func (k drainKit) receipts(t *testing.T, rule string) []scopedReceipt {
	t.Helper()
	rows, err := k.f.Pool.Query(context.Background(), `SELECT r.event_type,r.revision,COALESCE((r.payload->>'coalesced_count')::int,1),r.processed_at IS NOT NULL
 FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.workspace_id=$1 AND w.default_rule_key=$2 ORDER BY r.revision,r.created_at`, k.f.WorkspaceID, rule)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []scopedReceipt
	for rows.Next() {
		var r scopedReceipt
		if err := rows.Scan(&r.Type, &r.Revision, &r.Count, &r.Processed); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (k drainKit) agentComment(t *testing.T) string {
	t.Helper()
	return k.f.Comment(t, k.issue, "from the agent", testutil.Cols{"author_type": "agent", "author_id": k.agent})
}

func TestScopedDrainCreatesInstanceForTheFirstEvent(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "first event, before any instance exists")
	if len(k.instances(t, k.issue)) != 0 {
		t.Fatal("no instance may exist before the drain")
	}
	k.drain(t)
	inst := k.instances(t, k.issue)
	if len(inst) != 1 {
		t.Fatalf("instances = %+v, want one", inst)
	}
	i := inst[0]
	if i.Rule != scopedRuleA || i.ScopeKind != "workspace" || i.ScopeID != k.f.WorkspaceID || !i.Enabled || i.Agent != k.agent || i.CreatedBy != k.owner ||
		i.Instruction != "look at it" || i.Mode != "continuous" || !slices.Equal(i.Events, []string{"comment.created"}) || i.Fingerprint == nil || *i.Fingerprint == "" {
		t.Fatalf("instance = %+v", i)
	}
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Type != "comment.created" || got[0].Processed || got[0].Count != 1 {
		t.Fatalf("receipts = %+v, want one pending comment.created", got)
	}
	k.wantOutcomes(t, "delivered")
}

func TestScopedDrainReplayIsIdempotent(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "once")
	k.drain(t)
	// A crash after delivery but before the mark leaves the input pending again.
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET handled_at=NULL,outcome=NULL WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.drain(t)
	k.drain(t)
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("receipts = %+v, want exactly one uncounted-twice receipt", got)
	}
	if got := len(k.instances(t, k.issue)); got != 1 {
		t.Fatalf("instances = %d, want 1", got)
	}
	k.wantOutcomes(t, "delivered")
}

func TestScopedDrainCoalescesAndKeepsMixedActorsAsNotOwn(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "member one")
	k.comment(t, "member two")
	k.agentComment(t)
	k.drain(t)
	got := k.receipts(t, scopedRuleA)
	if len(got) != 1 || got[0].Count != 3 {
		t.Fatalf("receipts = %+v, want one receipt coalescing three events", got)
	}
	k.wantOutcomes(t, "delivered", "delivered", "delivered")
	var payload []byte
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT r.payload FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.workspace_id=$1`, k.f.WorkspaceID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	// The latest actor is the agent, but a coalesced batch is never its own work.
	if _, _, single := receiptActor(db.IssueWakeupReceipt{Payload: payload}); single {
		t.Fatalf("a mixed batch must not read as a single actor's own work: %s", payload)
	}
}

func TestScopedDrainSingleAgentEventKeepsItsActor(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.agentComment(t)
	k.drain(t)
	var payload []byte
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT r.payload FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.workspace_id=$1`, k.f.WorkspaceID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	actorType, actorID, single := receiptActor(db.IssueWakeupReceipt{Payload: payload})
	if actorType != "agent" || actorID != k.agent || !single {
		t.Fatalf("actor = %s/%s single=%v, want the agent that wrote the comment: %s", actorType, actorID, single, payload)
	}
}

// Once the instance exists live capture delivers at once; the drain must add
// nothing on top of it, not even a count, and must not make an older event the
// receipt's latest.
func TestScopedDrainAfterLiveCaptureDoesNotDoubleCount(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "creates the instance")
	k.drain(t)
	k.comment(t, "captured live, and in the outbox")
	k.comment(t, "captured live, second")
	last := k.comment(t, "captured live, latest")
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != 4 {
		t.Fatalf("live capture receipts = %+v, want one with count 4", got)
	}
	k.drain(t)
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != 4 {
		t.Fatalf("receipts after drain = %+v, want the count unchanged", got)
	}
	var latest string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT r.payload->>'comment_id' FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.workspace_id=$1`, k.f.WorkspaceID).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if latest != last {
		t.Fatalf("the receipt's latest reference moved to %s, want the newest comment %s", latest, last)
	}
	k.wantOutcomes(t, "delivered", "delivered", "delivered", "delivered")
}

// An instance that exists but was switched to another configuration after live
// capture served the event is a different revision: the event is delivered to it.
func TestScopedDrainDeliversToTheNewRevisionAfterLiveCapture(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "creates the instance")
	k.drain(t)
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET config=$2::jsonb,revision=revision+1,updated_at=clock_timestamp() WHERE workspace_id=$1`,
		k.f.WorkspaceID, k.eventConfig("edited text", "comment.created"))
	k.comment(t, "live-captured under the old revision")
	k.drain(t)
	got := k.receipts(t, scopedRuleA)
	if len(got) != 2 || got[1].Revision != 2 || got[1].Count != 1 {
		t.Fatalf("receipts = %+v, want the event also pending under the new revision", got)
	}
}

func TestScopedDrainActivationWatermark(t *testing.T) {
	k := newDrainKit(t)
	disabled := `{"v":1,"enabled":false,"trigger":{"kind":"event","events":["comment.created"]},"target":{"type":"agent","id":"` + k.agent + `"},"instruction":"look at it"}`
	k.define(t, "workspace", k.f.WorkspaceID, scopedRuleA, true, disabled, "comment.created")
	k.comment(t, "while the rule is off")
	k.drain(t)
	k.wantOutcomes(t, "rule_disabled")

	// Enabling the rule is a write; an event captured before it is not new work.
	k.comment(t, "captured, then the rule is switched on")
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET config=$2::jsonb,revision=revision+1,updated_at=clock_timestamp() WHERE workspace_id=$1`,
		k.f.WorkspaceID, k.eventConfig("look at it", "comment.created"))
	k.drain(t)
	if len(k.instances(t, k.issue)) != 0 || len(k.receipts(t, scopedRuleA)) != 0 {
		t.Fatal("a pre-activation event became new work")
	}
	k.wantOutcomes(t, "rule_disabled", "definition_changed")

	k.comment(t, "after activation")
	k.drain(t)
	k.wantOutcomes(t, "rule_disabled", "definition_changed", "delivered")
	if got := k.receipts(t, scopedRuleA); len(got) != 1 {
		t.Fatalf("receipts = %+v, want only the post-activation event", got)
	}
}

func TestScopedDrainObsoleteScopeIsNeverReinterpretedUnderAnotherProject(t *testing.T) {
	k := newDrainKit(t)
	other := k.f.Project(t, "destination project")
	k.defineRoot(t, "project", k.project, scopedRuleA, "comment.created")
	k.defineRoot(t, "project", other, scopedRuleA, "comment.created")
	k.comment(t, "captured under the first project")
	k.f.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, k.issue, other)
	k.drain(t)
	k.wantOutcomes(t, "scope_changed")
	if len(k.instances(t, k.issue)) != 0 || len(k.receipts(t, scopedRuleA)) != 0 {
		t.Fatal("an event captured under one project was delivered under another")
	}

	k.comment(t, "after the move")
	k.drain(t)
	inst := k.instances(t, k.issue)
	if len(inst) != 1 || inst[0].ScopeKind != "project" || inst[0].ScopeID != other {
		t.Fatalf("instances = %+v, want one rooted in the destination project", inst)
	}
}

func TestScopedDrainDisposesEventsOfUnavailableIssues(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "closes before the drain")
	k.f.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, k.issue)
	k.drain(t)
	k.wantOutcomes(t, "issue_closed")

	k.f.Exec(t, `UPDATE issue SET status='todo' WHERE id=$1`, k.issue)
	k.comment(t, "parks in backlog before the drain")
	k.f.Exec(t, `UPDATE issue SET status='backlog' WHERE id=$1`, k.issue)
	k.drain(t)
	k.wantOutcomes(t, "issue_closed", "issue_dormant")

	k.f.Exec(t, `UPDATE issue SET status='todo' WHERE id=$1`, k.issue)
	k.comment(t, "deleted before the drain")
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET issue_id=gen_random_uuid() WHERE handled_at IS NULL AND workspace_id=$1`, k.f.WorkspaceID)
	k.drain(t)
	k.wantOutcomes(t, "issue_closed", "issue_dormant", "issue_gone")
	if len(k.instances(t, k.issue)) != 0 {
		t.Fatal("an unavailable issue gained an instance")
	}
}

func TestScopedDrainDefaultCapacityIsAnOutcomeNotAnError(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.f.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,default_rule_key,default_scope_kind,default_scope_id)
 SELECT gen_random_uuid(),$1,$2,$3,'default','event','continuous',gen_random_uuid()::text,'workspace',$1 FROM generate_series(1,32)`, k.f.WorkspaceID, k.issue, k.agent)
	k.comment(t, "the source write is unaffected")
	k.drain(t)
	k.wantOutcomes(t, "default_capacity")
	var enabled bool
	var reason *string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT enabled,capacity_reason FROM issue_wakeup WHERE issue_id=$1 AND default_rule_key=$2`, k.issue, scopedRuleA).Scan(&enabled, &reason); err != nil {
		t.Fatal(err)
	}
	if enabled || reason == nil || *reason != "default" {
		t.Fatalf("the instance must stay unapplied with its reason, got enabled=%v reason=%v", enabled, reason)
	}
	if got := k.receipts(t, scopedRuleA); len(got) != 0 {
		t.Fatalf("a full pool delivered %+v", got)
	}
}

func TestScopedDrainMalformedDefinitionIsIsolatedAndFailsClosed(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.define(t, "workspace", k.f.WorkspaceID, scopedRuleB, true, `{"v":1,"bogus":true}`, "comment.created")
	k.comment(t, "one rule is broken")
	k.drain(t)
	inst := k.instances(t, k.issue)
	if len(inst) != 1 || inst[0].Rule != scopedRuleA {
		t.Fatalf("instances = %+v, want only the healthy rule", inst)
	}
	k.wantOutcomes(t, "delivered")

	k.f.Exec(t, `DELETE FROM issue_wakeup_definition WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, scopedRuleA)
	k.comment(t, "only the broken rule is left")
	k.drain(t)
	k.wantOutcomes(t, "delivered", "invalid_definition")
}

func TestScopedDrainStaleSelectorDoesNotMatch(t *testing.T) {
	k := newDrainKit(t)
	// The stored selector is conservative; the resolved trigger decides.
	k.define(t, "workspace", k.f.WorkspaceID, scopedRuleA, true, k.eventConfig("look at it", "task.failed"), "comment.created")
	k.comment(t, "selector says yes, trigger says no")
	k.drain(t)
	k.wantOutcomes(t, "no_match")
	if len(k.instances(t, k.issue)) != 0 {
		t.Fatal("a non-matching rule gained an instance")
	}
}

func TestScopedDrainResolvesOverridesOfTheChain(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.define(t, "project", k.project, scopedRuleA, false, `{"v":1,"instruction":"project text"}`)
	k.comment(t, "project override applies")
	k.drain(t)
	inst := k.instances(t, k.issue)
	if len(inst) != 1 || inst[0].Instruction != "project text" || inst[0].ScopeKind != "workspace" {
		t.Fatalf("instances = %+v, want the project's instruction on the workspace rule's instance", inst)
	}
	k.define(t, "issue", k.issue, scopedRuleA, false, `{"v":1,"enabled":false}`)
	k.comment(t, "issue disables it")
	k.drain(t)
	k.wantOutcomes(t, "delivered", "rule_disabled")
}

func TestScopedDrainRebasesInstanceOnConfigurationChange(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "first")
	k.drain(t)
	before := k.instances(t, k.issue)[0]
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET config=$2::jsonb,revision=revision+1,updated_at=clock_timestamp() WHERE workspace_id=$1`,
		k.f.WorkspaceID, k.eventConfig("edited text", "comment.created"))
	k.comment(t, "after the edit")
	k.drain(t)
	after := k.instances(t, k.issue)
	if len(after) != 1 || after[0].ID != before.ID {
		t.Fatalf("the instance identity must survive an edit: %+v -> %+v", before, after)
	}
	if after[0].Instruction != "edited text" || after[0].Revision != before.Revision+1 || *after[0].Fingerprint == *before.Fingerprint {
		t.Fatalf("instance not rebased: %+v -> %+v", before, after[0])
	}
	got := k.receipts(t, scopedRuleA)
	if last := got[len(got)-1]; last.Revision != after[0].Revision || last.Processed {
		t.Fatalf("receipts = %+v, want the post-edit event pending under the new revision", got)
	}
}

func TestScopedDrainNeverDeliversToRulesCreatedAfterTheEvent(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "before the local rule exists")
	local := wakeCreate(t, k.f, k.s, parseTestUUID(t, k.issue), WakeupInput{AgentID: k.agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "local"})
	k.drain(t)
	if got := k.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1`, local.ID); got != 0 {
		t.Fatalf("the drain delivered a past event to a local rule created later: %d receipts", got)
	}
	if got := k.receipts(t, scopedRuleA); len(got) != 1 {
		t.Fatalf("default instance receipts = %+v, want 1", got)
	}
}

// probeTx watches the statements of a transaction and every savepoint under it:
// it counts the named queries that run and fails the ones that match.
type probeTx struct {
	pgx.Tx
	probe *sqlProbe
}

type sqlProbe struct {
	mu     sync.Mutex
	counts map[string]int
	// failQuery names the query to fail; failArg, when set, restricts the failure
	// to statements that carry that argument.
	failQuery string
	failArg   string
	err       error
}

func (p *sqlProbe) seen(sql string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.counts == nil {
		p.counts = map[string]int{}
	}
	if i := strings.Index(sql, "-- name: "); i >= 0 {
		name := strings.Fields(sql[i+len("-- name: "):])[0]
		p.counts[name]++
	}
}

func (p *sqlProbe) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[name]
}

func (p *sqlProbe) fails(sql string, args []any) bool {
	if p.failQuery == "" || !strings.Contains(sql, "-- name: "+p.failQuery) {
		return false
	}
	if p.failArg == "" {
		return true
	}
	for _, a := range args {
		if s, ok := a.(string); ok && s == p.failArg {
			return true
		}
	}
	return false
}

func (tx probeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.probe.seen(sql)
	if tx.probe.fails(sql, args) {
		return failedRow{tx.probe.err}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx probeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.probe.seen(sql)
	if tx.probe.fails(sql, args) {
		return nil, tx.probe.err
	}
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx probeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.probe.seen(sql)
	if tx.probe.fails(sql, args) {
		return pgconn.CommandTag{}, tx.probe.err
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx probeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	inner, err := tx.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return probeTx{Tx: inner, probe: tx.probe}, nil
}

type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

type probeStarter struct {
	TxStarter
	probe *sqlProbe
}

func (s probeStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return probeTx{Tx: tx, probe: s.probe}, nil
}

func (k drainKit) probed(probe *sqlProbe) *IssueWakeupService {
	return &IssueWakeupService{Tasks: &TaskService{Queries: k.s.Tasks.Queries, TxStarter: probeStarter{TxStarter: k.f.Pool, probe: probe}, Bus: k.s.Tasks.Bus}}
}

func TestScopedDrainRealDatabaseErrorsStayRetryable(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	second := k.f.Issue(t, "second issue", testutil.Cols{"project_id": k.project})
	k.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", second)
	k.comment(t, "creates the first instance")
	k.drain(t)
	k.comment(t, "healthy: instance exists")
	k.f.Comment(t, second, "unhealthy: creation fails")

	boom := errors.New("injected database failure")
	err := k.probed(&sqlProbe{failQuery: "CreateDefaultWakeupInstance", err: boom}).DrainScopedEvents(context.Background(), parseTestUUID(t, k.f.WorkspaceID))
	if !errors.Is(err, boom) {
		t.Fatalf("drain error = %v, want the database failure to surface", err)
	}
	// The healthy input of the same batch was still handled.
	var handled, retrying int
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE handled_at IS NOT NULL),count(*) FILTER (WHERE handled_at IS NULL AND retry_at > now()) FROM wakeup_scoped_event WHERE workspace_id=$1`, k.f.WorkspaceID).Scan(&handled, &retrying); err != nil {
		t.Fatal(err)
	}
	if handled != 2 || retrying != 1 {
		t.Fatalf("handled=%d retrying=%d, want the failed input left pending with a retry time and the others handled", handled, retrying)
	}
	if len(k.instances(t, second)) != 0 {
		t.Fatal("a failed creation left an instance behind")
	}

	// Waiting for its retry time, then succeeding.
	k.drain(t)
	if got := k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID); got != 1 {
		t.Fatalf("the input must wait for its retry time, pending=%d", got)
	}
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET retry_at=now()-interval '1 second' WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID)
	k.drain(t)
	if len(k.instances(t, second)) != 1 {
		t.Fatal("the retried input did not create its instance")
	}
}

func TestScopedDrainBatchIsBoundedAndSkipsLockedRows(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	for range scopedDrainBatch + 20 {
		k.comment(t, "burst")
	}
	k.drain(t)
	if got := k.pending(t); got != 20 {
		t.Fatalf("pending after one pass = %d, want 20: a pass is bounded to %d inputs", got, scopedDrainBatch)
	}

	// Rows another scheduler holds are skipped, not waited for.
	ctx := context.Background()
	tx, err := k.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := db.New(tx).ClaimWakeupScopedEvents(ctx, db.ClaimWakeupScopedEventsParams{Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, Oldest: pgtype.Timestamptz{Time: time.Now().Add(-scopedEventRetention), Valid: true}, SkipIssues: []pgtype.UUID{}, AfterAt: pgtype.Timestamptz{Valid: true}, AfterID: pgtype.UUID{Valid: true}, UntilAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}, UntilID: pgtype.UUID{Bytes: [16]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, Valid: true}, BatchSize: 100}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- k.s.DrainScopedEvents(ctx, parseTestUUID(t, k.f.WorkspaceID)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drain waited on rows another transaction holds")
	}
	if got := k.pending(t); got != 20 {
		t.Fatalf("pending = %d, want the 20 locked inputs untouched", got)
	}
	tx.Rollback(ctx)
	k.drain(t)
	if got := k.pending(t); got != 0 {
		t.Fatalf("pending = %d after the lock was released", got)
	}
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != scopedDrainBatch+20 {
		t.Fatalf("receipts = %+v, want every event counted once", got)
	}
}

func TestScopedDrainConcurrentSchedulersHandleEachInputOnce(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	const events = 90
	for range events {
		k.comment(t, "burst")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				if err := k.s.DrainScopedEvents(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		// Competing schedulers may lose a lock race; nothing may be lost.
		t.Logf("drain: %v", err)
	}
	k.drain(t)
	k.drain(t)
	if got := k.pending(t); got != 0 {
		t.Fatalf("pending = %d", got)
	}
	if got := len(k.instances(t, k.issue)); got != 1 {
		t.Fatalf("instances = %d, want 1: concurrent schedulers must not create two", got)
	}
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != events {
		t.Fatalf("receipts = %+v, want one receipt counting all %d events", got, events)
	}
}

func TestScopedEventRetentionIsBoundedAndVisible(t *testing.T) {
	k := newDrainKit(t)
	ctx := context.Background()
	old := time.Now().Add(-8 * 24 * time.Hour)
	ws := parseTestUUID(t, k.f.WorkspaceID)
	// 250 pending inputs past retention, 250 handled ones, one fresh.
	k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,event_type,event_key,payload,captured_at)
 SELECT $1,$2,'comment.created',gen_random_uuid()::text,'{}'::jsonb,$3 FROM generate_series(1,250)`, k.f.WorkspaceID, k.issue, old)
	k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,event_type,event_key,payload,captured_at,handled_at,outcome)
 SELECT $1,$2,'comment.created',gen_random_uuid()::text,'{}'::jsonb,$3,$3,'delivered' FROM generate_series(1,250)`, k.f.WorkspaceID, k.issue, old)
	k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,event_type,event_key,payload,captured_at) VALUES($1,$2,'comment.created','fresh','{}'::jsonb,now())`, k.f.WorkspaceID, k.issue)

	if err := k.s.pruneScopedEvents(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND outcome='expired'`, k.f.WorkspaceID); got != scopedPruneBatch {
		t.Fatalf("expired in one pass = %d, want the bound %d", got, scopedPruneBatch)
	}
	if got := k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND outcome='delivered'`, k.f.WorkspaceID); got != 250-scopedPruneBatch {
		t.Fatalf("handled rows left = %d, want %d: a pass deletes at most %d", got, 250-scopedPruneBatch, scopedPruneBatch)
	}
	for range 3 {
		if err := k.s.pruneScopedEvents(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if got := k.pending(t); got != 1 {
		t.Fatalf("pending = %d, want only the fresh input", got)
	}
	// Expiry is accounted for, not silent: closed inputs stay visible as such
	// until their own retention ends.
	acc, err := k.s.ScopedEventAccounting(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if acc["expired"] != 250 || acc["pending"] != 1 {
		t.Fatalf("accounting = %v, want 250 expired and 1 pending", acc)
	}
}

func TestScopedDrainRunsInsideTheSchedulerTick(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "handled by the tick")
	if err := k.s.TickWorkspaces(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err != nil {
		t.Logf("tick: %v", err)
	}
	if got := k.pending(t); got != 0 {
		t.Fatalf("pending after a tick = %d", got)
	}
	k.wantOutcomes(t, "delivered")
}

func TestWakeupEventSelector(t *testing.T) {
	patch := func(raw string) WakeupConfigPatch {
		p, err := DecodeWakeupConfigPatch([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"no trigger", `{"v":1,"instruction":"x"}`, nil},
		{"built-in preset", `{"v":1,"trigger":{"kind":"child_done"}}`, nil},
		{"events sorted and unique", `{"v":1,"trigger":{"kind":"event","events":["task.failed","comment.created","comment.created"]}}`, []string{"comment.created", "task.failed"}},
		{"unknown event dropped", `{"v":1,"trigger":{"kind":"event","events":["comment.created","not.an.event"]}}`, []string{"comment.created"}},
		{"cleared trigger", `{"v":1,"trigger":null}`, nil},
		{"malformed trigger", `{"v":1,"trigger":{"kind":"event","events":"comment.created"}}`, nil},
	}
	for _, c := range cases {
		if got := WakeupEventSelector(patch(c.raw)); !slices.Equal(got, c.want) {
			t.Errorf("%s: selector = %v, want %v", c.name, got, c.want)
		}
	}
}

// A definition written through the API stores the selector its trigger
// selects. No event trigger is accepted yet, so a built-in stores none.
func TestWakeupDefinitionWriteStoresTheEventSelector(t *testing.T) {
	k := newDrainKit(t)
	ws := parseTestUUID(t, k.f.WorkspaceID)
	ref := WakeupScopeRef{Kind: WakeupScopeProject, ID: parseTestUUID(t, k.project), WorkspaceID: ws}
	var patch WakeupConfigPatch
	patch.Instruction = wakeupField[string]{Set: true, Value: "project text"}
	if _, err := k.s.SaveWakeupDefinition(context.Background(), ref, parseTestUUID(t, k.owner), WakeupDefinitionWrite{RuleKey: SystemRulePRMerged, Patch: patch}); err != nil {
		t.Fatal(err)
	}
	var selector []string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT event_types FROM issue_wakeup_definition WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, SystemRulePRMerged).Scan(&selector); err != nil {
		t.Fatal(err)
	}
	if len(selector) != 0 {
		t.Fatalf("a built-in definition selects no ordinary event, stored %v", selector)
	}
	// Event triggers stay unavailable until a later layer implements them.
	var event WakeupConfigPatch
	event.Trigger = wakeupObject{Set: true, Value: []byte(`{"kind":"event","events":["comment.created"]}`)}
	event.Instruction = wakeupField[string]{Set: true, Value: "text"}
	_, err := k.s.SaveWakeupDefinition(context.Background(), ref, parseTestUUID(t, k.owner), WakeupDefinitionWrite{RuleKey: scopedRuleA, Patch: event})
	if !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("an event trigger must still be refused, got %v", err)
	}
}

// A burst on one issue is claimed together and resolved once: the issue is
// locked and its definitions loaded one time for all of its inputs.
func TestScopedDrainResolvesABurstOnOneIssueOnce(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	const burst = 30
	for range burst {
		k.comment(t, "burst")
	}
	probe := &sqlProbe{}
	if err := k.probed(probe).DrainScopedEvents(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
	if got := k.pending(t); got != 0 {
		t.Fatalf("pending = %d, want the whole burst handled in one pass", got)
	}
	if locks, loads, creates := probe.count("TryLockWakeupIssue"), probe.count("ListWakeupDefinitionsInScope"), probe.count("CreateDefaultWakeupInstance"); locks != 1 || loads != 3 || creates != 1 {
		t.Fatalf("issue locks=%d definition loads=%d instance creations=%d, want 1, 3 (workspace, project and issue scope) and 1 for %d inputs", locks, loads, creates, burst)
	}
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != burst {
		t.Fatalf("receipts = %+v, want one receipt coalescing the burst", got)
	}
}

// One input that cannot be handled holds back only itself: the rest of its
// issue's inputs are claimed again and delivered.
func TestScopedDrainAPoisonedInputDoesNotBlockItsIssue(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	for range 3 {
		k.comment(t, "burst")
	}
	var poisoned string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT event_key FROM wakeup_scoped_event WHERE workspace_id=$1 ORDER BY captured_at,id OFFSET 1 LIMIT 1`, k.f.WorkspaceID).Scan(&poisoned); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("injected replay failure")
	err := k.probed(&sqlProbe{failQuery: "ReplayScopedWakeupEvent", failArg: poisoned, err: boom}).DrainScopedEvents(context.Background(), parseTestUUID(t, k.f.WorkspaceID))
	if !errors.Is(err, boom) {
		t.Fatalf("drain error = %v, want the injected failure", err)
	}
	var stuck, retrying int
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE handled_at IS NULL),count(*) FILTER (WHERE handled_at IS NULL AND retry_at>now() AND event_key=$2) FROM wakeup_scoped_event WHERE workspace_id=$1`, k.f.WorkspaceID, poisoned).Scan(&stuck, &retrying); err != nil {
		t.Fatal(err)
	}
	if stuck != 1 || retrying != 1 {
		t.Fatalf("pending=%d retrying=%d, want only the poisoned input left, waiting for its retry", stuck, retrying)
	}
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("receipts = %+v, want the two healthy events delivered", got)
	}
	if got := len(k.instances(t, k.issue)); got != 1 {
		t.Fatalf("instances = %d, want 1: the rolled-back attempt must leave none behind", got)
	}
}

// An issue another writer holds is contention, not a fault: the drain does not
// wait for it, writes nothing for it, reports no error and leaves the input
// pending. The scan moves past it and wraps, so the input is delivered by the
// next pass once the issue is free.
func TestScopedDrainWaitsOutAnIssueAnotherWriterHolds(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "the issue is busy")
	ctx := context.Background()
	holder, err := k.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM issue WHERE id=$1 FOR NO KEY UPDATE`, k.issue); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := k.s.DrainScopedEvents(ctx, parseTestUUID(t, k.f.WorkspaceID)); err != nil {
		t.Fatalf("contention must not surface as an error: %v", err)
	}
	if took := time.Since(started); took > 150*time.Millisecond {
		t.Fatalf("the drain waited %v for a held issue", took)
	}
	if k.pending(t) != 1 || k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND retry_at IS NOT NULL`, k.f.WorkspaceID) != 0 {
		t.Fatal("a busy issue's input must stay pending and untouched")
	}
	holder.Rollback(ctx)
	k.drain(t)
	k.wantOutcomes(t, "delivered")
}
