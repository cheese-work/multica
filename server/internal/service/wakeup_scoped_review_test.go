package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Regressions for the signing review of L9 head 0c752ff6b (CHE-1199): each one
// reproduced a failure there and is kept as a maintained test.

// Deleting or resetting an override changes the rule an input was captured
// under. The boundary must survive the deletion, not only an in-place update.
func TestScopedDrainDeletedOverrideKeepsTheActivationBoundary(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.define(t, "project", k.project, scopedRuleA, false, `{"v":1,"enabled":false}`)
	k.comment(t, "event while the rule was disabled for this project")
	ref := WakeupScopeRef{Kind: WakeupScopeProject, ID: parseTestUUID(t, k.project), WorkspaceID: parseTestUUID(t, k.f.WorkspaceID)}
	if err := k.s.DeleteWakeupDefinition(context.Background(), ref, scopedRuleA, 1); err != nil {
		t.Fatal(err)
	}
	k.drain(t)
	k.wantOutcomes(t, "definition_changed")
	if len(k.receipts(t, scopedRuleA)) != 0 || len(k.instances(t, k.issue)) != 0 {
		t.Fatal("a pre-activation input became work after the override was reset")
	}
	k.comment(t, "after the reset")
	k.drain(t)
	k.wantOutcomes(t, "definition_changed", "delivered")
}

// A deleted and recreated definition is a new activation even though its
// revision number starts over.
func TestScopedDrainRecreatedOverrideKeepsTheActivationBoundary(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.define(t, "project", k.project, scopedRuleA, false, `{"v":1,"enabled":false}`)
	k.comment(t, "captured under the first override")
	k.f.Exec(t, `DELETE FROM issue_wakeup_definition WHERE workspace_id=$1 AND scope_kind='project'`, k.f.WorkspaceID)
	k.define(t, "project", k.project, scopedRuleA, false, `{"v":1,"enabled":false}`)
	k.drain(t)
	k.wantOutcomes(t, "definition_changed")
}

// Inputs older than the retention window expire; they are never delivered,
// whatever the size of the expired backlog.
func TestScopedExpiredInputsAreNeverDelivered(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET updated_at=now()-interval '10 days' WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.comment(t, "event before a prolonged drain outage")
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET captured_at=now()-interval '8 days' WHERE workspace_id=$1`, k.f.WorkspaceID)
	if err := k.s.TickWorkspaces(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err != nil {
		t.Logf("tick: %v", err)
	}
	k.wantOutcomes(t, "expired")
	if len(k.receipts(t, scopedRuleA)) != 0 || len(k.instances(t, k.issue)) != 0 {
		t.Fatal("an expired input became work")
	}
}

func TestScopedExpiredBacklogBeyondThePruneBoundIsNeverDelivered(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET updated_at=now()-interval '10 days' WHERE workspace_id=$1`, k.f.WorkspaceID)
	for range 3 {
		k.comment(t, "old")
	}
	// More expired inputs than one prune pass can close.
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET captured_at=now()-interval '8 days' WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,project_id,event_type,event_key,payload,chain,captured_at)
 SELECT workspace_id,issue_id,project_id,event_type,gen_random_uuid()::text,payload,chain,captured_at FROM wakeup_scoped_event, generate_series(1,100) WHERE workspace_id=$1`, k.f.WorkspaceID)
	if got := k.pending(t); got <= scopedPruneBatch {
		t.Fatalf("fixture has %d expired inputs, want more than the prune bound %d", got, scopedPruneBatch)
	}
	ws := parseTestUUID(t, k.f.WorkspaceID)
	if err := k.s.pruneScopedEvents(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	k.drain(t)
	if len(k.receipts(t, scopedRuleA)) != 0 || len(k.instances(t, k.issue)) != 0 {
		t.Fatal("an expired input beyond the first prune pass became work")
	}
	for range 3 {
		if err := k.s.TickWorkspaces(context.Background(), ws); err != nil {
			t.Logf("tick: %v", err)
		}
	}
	if got := k.pending(t); got != 0 {
		t.Fatalf("pending = %d, want every expired input closed", got)
	}
	if len(k.receipts(t, scopedRuleA)) != 0 {
		t.Fatal("an expired input became a receipt")
	}
}

// Closing an input as expired must not relabel one a drain delivers at the
// same moment: the expiry waits for neither its lock nor its row.
func TestScopedExpiryNeverOverwritesAnInputBeingDelivered(t *testing.T) {
	k := newDrainKit(t)
	k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,event_type,event_key,payload,captured_at)
 VALUES($1,$2,'comment.created','k','{}'::jsonb,now()-interval '8 days')`, k.f.WorkspaceID, k.issue)
	ctx := context.Background()
	drain, err := k.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer drain.Rollback(ctx)
	if _, err := drain.Exec(ctx, `UPDATE wakeup_scoped_event SET handled_at=now(),outcome='delivered' WHERE workspace_id=$1`, k.f.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := k.s.Tasks.Queries.ExpireWakeupScopedEvents(ctx, db.ExpireWakeupScopedEventsParams{
			Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, Before: pgtype.Timestamptz{Time: time.Now().Add(-scopedEventRetention), Valid: true}, BatchSize: scopedPruneBatch,
		})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := drain.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	k.wantOutcomes(t, "delivered")
}

// A retried older input must not displace newer live evidence, and the receipt
// keeps the true earliest time and the full count.
func TestScopedDrainLateOlderInputKeepsNewerLiveEvidence(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	older := k.comment(t, "older pending input")
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET retry_at=now()+interval '1 minute' WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.comment(t, "materializes the instance")
	k.drain(t)
	newest := k.comment(t, "newest live-captured input")
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET retry_at=NULL WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.drain(t)
	var comment string
	var first, olderAt time.Time
	var count int
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT r.payload->>'comment_id',(r.payload->>'first_occurred_at')::timestamptz,(r.payload->>'coalesced_count')::int FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.workspace_id=$1 AND r.processed_at IS NULL`, k.f.WorkspaceID).Scan(&comment, &first, &count); err != nil {
		t.Fatal(err)
	}
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT min(captured_at) FROM wakeup_scoped_event WHERE workspace_id=$1`, k.f.WorkspaceID).Scan(&olderAt); err != nil {
		t.Fatal(err)
	}
	if comment != newest {
		t.Fatalf("receipt names comment %s, want the newest %s (older was %s)", comment, newest, older)
	}
	if count != 3 || !first.Equal(olderAt) {
		t.Fatalf("count=%d first_occurred_at=%s, want 3 and the earliest event %s", count, first, olderAt)
	}
}

// One locked issue is skipped for the pass: it neither stalls other issues and
// workspaces nor turns into a reported fault.
func TestScopedDrainBusyIssueDoesNotStallOtherWork(t *testing.T) {
	busy := newDrainKit(t)
	healthy := newDrainKit(t)
	busy.defineRoot(t, "workspace", busy.f.WorkspaceID, scopedRuleA, "comment.created")
	healthy.defineRoot(t, "workspace", healthy.f.WorkspaceID, scopedRuleA, "comment.created")
	busy.comment(t, "oldest")
	healthy.comment(t, "another workspace")
	ctx := context.Background()
	holder, err := busy.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM issue WHERE id=$1 FOR NO KEY UPDATE`, busy.issue); err != nil {
		t.Fatal(err)
	}
	if err := busy.s.DrainScopedEvents(ctx, parseTestUUID(t, busy.f.WorkspaceID), parseTestUUID(t, healthy.f.WorkspaceID)); err != nil {
		t.Fatalf("contention must not surface as an error: %v", err)
	}
	if healthy.pending(t) != 0 {
		t.Fatal("the oldest locked issue stalled an unrelated workspace in the same pass")
	}
	if busy.pending(t) != 1 {
		t.Fatalf("busy pending = %d, want the locked input left for a later pass", busy.pending(t))
	}
	holder.Rollback(ctx)
	busy.drain(t)
	busy.wantOutcomes(t, "delivered")
}

// A stored setting that no instance can hold isolates its own rule before any
// database write; it never rolls back a healthy rule's delivery.
func TestScopedDrainInvalidSettingsIsolateTheirRule(t *testing.T) {
	for name, patch := range map[string]string{
		"mode":      `{"mode":"not-a-mode"}`,
		"max_fires": `{"max_fires":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			k := newDrainKit(t)
			k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
			k.define(t, "workspace", k.f.WorkspaceID, scopedRuleB, true, k.eventConfig("bad setting", "comment.created"), "comment.created")
			k.f.Exec(t, `UPDATE issue_wakeup_definition SET config=config || $3::jsonb WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, scopedRuleB, patch)
			k.comment(t, "the valid rule must still receive this")
			if err := k.s.DrainScopedEvents(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err != nil {
				t.Fatalf("a malformed rule escaped as a database error: %v", err)
			}
			k.wantOutcomes(t, "delivered")
			if got := k.receipts(t, scopedRuleA); len(got) != 1 {
				t.Fatalf("valid rule receipts = %+v, want 1", got)
			}
			if got := k.instances(t, k.issue); len(got) != 1 || got[0].Rule != scopedRuleA {
				t.Fatalf("instances = %+v, want only the valid rule's", got)
			}
		})
	}
}

// Held issues are passed over without a wait or a write, so five of them do not
// starve healthy work behind them even in back-to-back passes, and once the locks
// are gone the next pass delivers them.
func TestScopedDrainPersistentlyBusyIssuesDoNotStarveHealthyWork(t *testing.T) {
	const busyIssues = 5
	ctx := context.Background()
	var kits []drainKit
	var scope []pgtype.UUID
	for range busyIssues {
		k := newDrainKit(t)
		k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
		k.comment(t, "oldest, on a held issue")
		kits = append(kits, k)
		scope = append(scope, parseTestUUID(t, k.f.WorkspaceID))
	}
	healthy := newDrainKit(t)
	healthy.defineRoot(t, "workspace", healthy.f.WorkspaceID, scopedRuleA, "comment.created")
	healthy.comment(t, "newer, in an unrelated workspace")
	scope = append(scope, parseTestUUID(t, healthy.f.WorkspaceID))
	// One transaction holds all five issues: the shared test pool has as many
	// connections as the machine has CPUs (four on CI), so one held transaction per
	// issue would leave the drain without a connection.
	holder, err := healthy.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Rollback(ctx) })
	for _, k := range kits {
		if _, err := holder.Exec(ctx, `SELECT 1 FROM issue WHERE id=$1 FOR NO KEY UPDATE`, k.issue); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	if err := healthy.s.DrainScopedEvents(ctx, scope...); err != nil {
		t.Fatalf("contention must not surface as an error: %v", err)
	}
	if healthy.pending(t) != 0 {
		t.Fatal("five held issues starved unrelated work in the first pass, with every lock still held")
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("the pass took %v: held issues must not be waited for", took)
	}
	healthy.wantOutcomes(t, "delivered")
	for _, k := range kits {
		if k.pending(t) != 1 {
			t.Fatalf("a held issue's input must stay pending, got %d", k.pending(t))
		}
	}
	holder.Rollback(ctx)
	if err := healthy.s.DrainScopedEvents(ctx, scope...); err != nil {
		t.Fatal(err)
	}
	for _, k := range kits {
		k.wantOutcomes(t, "delivered")
	}
}

// With more held issues at the head than one pass will note, the passes at the
// scheduler's cadence move past the ones already tried, so healthy work behind
// them is reached while every lock is still held.
func TestScopedDrainBusyHeadLongerThanOnePassStillProgresses(t *testing.T) {
	old := scopedBusyAttempts
	scopedBusyAttempts = 3
	t.Cleanup(func() { scopedBusyAttempts = old })
	const busyIssues = 8
	ctx := context.Background()
	var kits []drainKit
	var scope []pgtype.UUID
	for range busyIssues {
		k := newDrainKit(t)
		k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
		k.comment(t, "older input on a held issue")
		kits = append(kits, k)
		scope = append(scope, parseTestUUID(t, k.f.WorkspaceID))
	}
	healthy := newDrainKit(t)
	healthy.defineRoot(t, "workspace", healthy.f.WorkspaceID, scopedRuleA, "comment.created")
	healthy.comment(t, "healthy input behind eight held issues")
	scope = append(scope, parseTestUUID(t, healthy.f.WorkspaceID))
	holder, err := healthy.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Rollback(ctx) })
	for _, k := range kits {
		if _, err := holder.Exec(ctx, `SELECT 1 FROM issue WHERE id=$1 FOR NO KEY UPDATE`, k.issue); err != nil {
			t.Fatal(err)
		}
	}
	for tick := 1; tick <= 4 && healthy.pending(t) > 0; tick++ {
		if err := healthy.s.TickWorkspaces(ctx, scope...); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}
	if healthy.pending(t) != 0 {
		t.Fatal("a held head longer than one pass starved healthy work across four 30 s ticks")
	}
	healthy.wantOutcomes(t, "delivered")
}

// A busy delay never extends retention: an input past seven days expires however
// recently its issue was found busy.
func TestScopedBusyDelayDoesNotOutliveRetention(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "held, then too old")
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET captured_at=now()-interval '8 days',retry_at=now()+interval '1 minute' WHERE workspace_id=$1`, k.f.WorkspaceID)
	if err := k.s.pruneScopedEvents(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	k.wantOutcomes(t, "expired")
}

// Five persistently held issues must not starve healthy work at the scheduler's
// real 30 s cadence, including a late tick, with every lock still held and no
// edit of the inputs. The healthy input is delivered before the locks release.
func TestScopedDrainBusyIssuesAtSchedulerCadence(t *testing.T) {
	ctx := context.Background()
	var kits []drainKit
	var scope []pgtype.UUID
	for range 5 {
		k := newDrainKit(t)
		k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
		k.comment(t, "older input on a held issue")
		kits = append(kits, k)
		scope = append(scope, parseTestUUID(t, k.f.WorkspaceID))
	}
	healthy := newDrainKit(t)
	healthy.defineRoot(t, "workspace", healthy.f.WorkspaceID, scopedRuleA, "comment.created")
	healthy.comment(t, "healthy input behind five held issues")
	scope = append(scope, parseTestUUID(t, healthy.f.WorkspaceID))
	holder, err := healthy.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Rollback(ctx) })
	for _, k := range kits {
		if _, err := holder.Exec(ctx, `SELECT 1 FROM issue WHERE id=$1 FOR NO KEY UPDATE`, k.issue); err != nil {
			t.Fatal(err)
		}
	}
	delivered := -1
	for tick := range 4 {
		if err := healthy.s.TickWorkspaces(ctx, scope...); err != nil {
			t.Fatalf("tick %d: contention must not surface as an error: %v", tick+1, err)
		}
		if delivered < 0 && healthy.pending(t) == 0 {
			delivered = tick + 1
		}
		for _, k := range kits {
			if k.pending(t) != 1 {
				t.Fatalf("tick %d: a held issue's input changed disposition with its lock held", tick+1)
			}
		}
	}
	if delivered < 0 {
		t.Fatal("five held issues starved healthy work across four scheduler ticks, every lock still held")
	}
	t.Logf("healthy input delivered at tick %d with all five locks held", delivered)
	healthy.wantOutcomes(t, "delivered")
	// Released, the held inputs deliver on a later tick without being edited.
	holder.Rollback(ctx)
	if err := healthy.s.TickWorkspaces(ctx, scope...); err != nil {
		t.Fatal(err)
	}
	for _, k := range kits {
		k.wantOutcomes(t, "delivered")
	}
}

// Busy deferral does not rewrite an issue's pending backlog: a burst far larger
// than the drain's budget on one held issue costs no outbox writes, and the
// backlog is delivered in bounded passes once the issue is free.
func TestScopedDrainBusyIssueBacklogIsNotRewritten(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "pending burst on one busy issue")
	k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,project_id,event_type,event_key,payload,chain,captured_at)
 SELECT workspace_id,issue_id,project_id,event_type,gen_random_uuid()::text,payload,chain,captured_at
 FROM wakeup_scoped_event,generate_series(1,500) WHERE workspace_id=$1`, k.f.WorkspaceID)
	ctx := context.Background()
	holder, err := k.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `SELECT 1 FROM issue WHERE id=$1 FOR NO KEY UPDATE`, k.issue); err != nil {
		t.Fatal(err)
	}
	if err := k.s.DrainScopedEvents(ctx, parseTestUUID(t, k.f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
	if written := k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND (retry_at IS NOT NULL OR handled_at IS NOT NULL)`, k.f.WorkspaceID); written > scopedDrainBatch {
		t.Fatalf("one pass over a held issue wrote %d outbox rows, beyond the %d-input budget", written, scopedDrainBatch)
	}
	holder.Rollback(ctx)
	for range 15 {
		if err := k.s.DrainScopedEvents(ctx, parseTestUUID(t, k.f.WorkspaceID)); err != nil {
			t.Fatal(err)
		}
	}
	if got := k.pending(t); got != 0 {
		t.Fatalf("pending = %d: a backlog larger than the bound must still drain in bounded passes", got)
	}
}

// heldPrefix puts n issues, each with one outbox input older than any other, in a
// workspace whose issue rows one transaction holds, and a healthy rule with one
// newer input in another workspace. The production bounds stay unchanged.
func heldPrefix(t *testing.T, n int) (held, healthy drainKit, holder pgx.Tx, scope []pgtype.UUID) {
	t.Helper()
	held = newDrainKit(t)
	held.f.Cleanup(t, `DELETE FROM issue WHERE workspace_id=$1 AND id<>$2`, held.f.WorkspaceID, held.issue)
	held.f.Exec(t, `WITH added AS (
 INSERT INTO issue(workspace_id,title,status,priority,creator_type,creator_id,number)
 SELECT $1,'held prefix','todo','none','member',$2,1000+n FROM generate_series(1,$3::int) n
 RETURNING id,workspace_id,number)
 INSERT INTO wakeup_scoped_event(workspace_id,issue_id,event_type,event_key,payload,captured_at)
 SELECT workspace_id,id,'comment.created','held-'||number,'{}',now()-interval '1 hour'+number*interval '1 microsecond' FROM added`, held.f.WorkspaceID, held.owner, n)
	healthy = newDrainKit(t)
	healthy.defineRoot(t, "workspace", healthy.f.WorkspaceID, scopedRuleA, "comment.created")
	healthy.comment(t, "healthy, behind the held prefix")
	ctx := context.Background()
	holder, err := held.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `SELECT 1 FROM issue WHERE workspace_id=$1 AND id<>$2 FOR NO KEY UPDATE`, held.f.WorkspaceID, held.issue); err != nil {
		t.Fatal(err)
	}
	return held, healthy, holder, []pgtype.UUID{parseTestUUID(t, held.f.WorkspaceID), parseTestUUID(t, healthy.f.WorkspaceID)}
}

func outboxFingerprint(t *testing.T, k drainKit) string {
	t.Helper()
	var sum string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT md5(string_agg(row_to_json(e)::text||e.xmin::text,',' ORDER BY e.id)) FROM wakeup_scoped_event e WHERE workspace_id=$1`, k.f.WorkspaceID).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

// A held prefix of any length, below, at and beyond the old 1,000-entry cache and
// whatever the spacing of the ticks, must not starve healthy work behind it: the
// real tick reaches it while every lock is still held, and the held inputs are
// neither rewritten nor lost, then deliver once their issues are free.
func TestScopedDrainHeldPrefixNeverStarvesHealthyWork(t *testing.T) {
	if testing.Short() {
		t.Skip("production-bound fixture")
	}
	// Progress is a scan position, not a timer: the spacing of the ticks (30 s,
	// late, or longer than the old 10-minute parking) cannot change it, so the
	// ticks run back to back and the sizes are the boundaries that mattered.
	for _, tc := range []struct {
		name string
		n    int
	}{
		{"below_old_cache_999", 999},
		{"at_old_cache_1000", 1000},
		{"beyond_old_cache_1050", 1050},
		{"short_prefix_120", 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held, healthy, holder, scope := heldPrefix(t, tc.n)
			ctx := context.Background()
			before := outboxFingerprint(t, held)
			delivered := -1
			for tick := 0; tick < 45 && delivered < 0; tick++ {
				if err := healthy.s.TickWorkspaces(ctx, scope...); err != nil {
					t.Fatalf("tick %d: contention must not surface as an error: %v", tick, err)
				}
				if healthy.pending(t) == 0 {
					delivered = tick
				}
			}
			if delivered < 0 {
				t.Fatalf("healthy work starved across 45 ticks behind %d held issues, every lock still held", tc.n)
			}
			t.Logf("%d held issues: healthy input delivered at tick %d", tc.n, delivered)
			if before != outboxFingerprint(t, held) || held.pending(t) != tc.n {
				t.Fatal("contention rewrote or lost held outbox rows")
			}
			holder.Rollback(ctx)
			for i := 0; i < tc.n/scopedDrainBatch+3 && held.pending(t) > 0; i++ {
				if err := healthy.s.DrainScopedEvents(ctx, scope...); err != nil {
					t.Fatal(err)
				}
			}
			if got := held.pending(t); got != 0 {
				t.Fatalf("%d held inputs still pending after their issues were free: every busy input must be retried", got)
			}
		})
	}
}

// A pass does a bounded amount of contention work however many issues are held.
func TestScopedDrainPassTriesAtMostTheBusyBoundOfHeldIssues(t *testing.T) {
	held, healthy, _, scope := heldPrefix(t, 200)
	probe := &sqlProbe{}
	if err := healthy.probed(probe).DrainScopedEvents(context.Background(), scope...); err != nil {
		t.Fatal(err)
	}
	if got := probe.count("TryLockWakeupIssue"); got > scopedBusyAttempts+1 {
		t.Fatalf("one pass tried %d issue locks over 200 held issues, want at most %d", got, scopedBusyAttempts+1)
	}
	if held.pending(t) != 200 {
		t.Fatal("held inputs must stay pending")
	}
}

// A steady stream of newer inputs never starves a held issue's input: the scan
// wraps, so once the issue is free its input is reached while the stream goes on.
func TestScopedDrainSteadyStreamDoesNotStarveHeldInputs(t *testing.T) {
	held, healthy, holder, scope := heldPrefix(t, 120)
	ctx := context.Background()
	for tick := range 6 {
		healthy.comment(t, "stream")
		if err := healthy.s.TickWorkspaces(ctx, scope...); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}
	if got := healthy.pending(t); got > 3 {
		t.Fatalf("%d stream inputs pending after six ticks behind a held prefix of 120", got)
	}
	holder.Rollback(ctx)
	for tick := range 6 {
		healthy.comment(t, "stream")
		if err := healthy.s.TickWorkspaces(ctx, scope...); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}
	if got := held.pending(t); got != 0 {
		t.Fatalf("%d held inputs still pending six ticks after their issues were free, with a stream of newer inputs", got)
	}
}

// A retention pass touches at most scopedPruneBatch rows of each kind whatever
// the table holds and whatever the planner knows about it: the bound must not
// depend on a freshly created table's statistics.
func TestScopedRetentionPassBoundHoldsAtAnyTableSizeAndStatistics(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rows    int
		analyze bool
	}{
		{"small_fresh", 250, false},
		{"small_analyzed", 250, true},
		{"medium_analyzed", 2000, true},
		{"large_analyzed", 20000, true},
		{"large_fresh_statistics", 20000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newDrainKit(t)
			ctx := context.Background()
			old := time.Now().Add(-8 * 24 * time.Hour)
			k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,event_type,event_key,payload,captured_at)
 SELECT $1,$2,'comment.created',gen_random_uuid()::text,'{}'::jsonb,$3 FROM generate_series(1,$4::int)`, k.f.WorkspaceID, k.issue, old, tc.rows)
			k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,event_type,event_key,payload,captured_at,handled_at,outcome)
 SELECT $1,$2,'comment.created',gen_random_uuid()::text,'{}'::jsonb,$3,$3,'delivered' FROM generate_series(1,$4::int)`, k.f.WorkspaceID, k.issue, old, tc.rows)
			if tc.analyze {
				k.f.Exec(t, `ANALYZE wakeup_scoped_event`)
			}
			if err := k.s.pruneScopedEvents(ctx, time.Now()); err != nil {
				t.Fatal(err)
			}
			if got := k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND outcome='expired'`, k.f.WorkspaceID); got != scopedPruneBatch {
				t.Fatalf("expired in one pass = %d, want exactly the bound %d", got, scopedPruneBatch)
			}
			if got := k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND outcome='delivered'`, k.f.WorkspaceID); got != tc.rows-scopedPruneBatch {
				t.Fatalf("handled rows left = %d, want %d: a pass deletes at most %d", got, tc.rows-scopedPruneBatch, scopedPruneBatch)
			}
		})
	}
}

// arriveClones adds n new pending inputs to the workspace of k by cloning its
// oldest input's captured evidence with fresh keys and times, so a stream can be
// supplied without the capture path; definitions and their stamps are unchanged.
func arriveClones(t *testing.T, k drainKit, n int) {
	t.Helper()
	k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,project_id,event_type,event_key,payload,chain,captured_at)
 SELECT workspace_id,issue_id,project_id,event_type,gen_random_uuid()::text,payload,chain,clock_timestamp()
 FROM (SELECT * FROM wakeup_scoped_event WHERE workspace_id=$1 ORDER BY captured_at,id LIMIT 1) seed CROSS JOIN generate_series(1,$2::int)`, k.f.WorkspaceID, n)
}

// scopedRevisitTicks is the bound the sweep promises: an older input whose issue
// is free is delivered within this many ticks however many inputs arrive per tick.
const scopedRevisitTicks = 3

func revisitUnderArrivals(t *testing.T, arrivals int, spacing time.Duration) {
	t.Helper()
	ctx := context.Background()
	old := newDrainKit(t)
	old.defineRoot(t, "workspace", old.f.WorkspaceID, scopedRuleA, "comment.created")
	old.comment(t, "older input while its issue is held")
	stream := newDrainKit(t)
	stream.defineRoot(t, "workspace", stream.f.WorkspaceID, scopedRuleA, "comment.created")
	stream.comment(t, "stream seed")
	arriveClones(t, stream, 49)
	scope := []pgtype.UUID{parseTestUUID(t, old.f.WorkspaceID), parseTestUUID(t, stream.f.WorkspaceID)}
	holder, err := old.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM issue WHERE id=$1 FOR NO KEY UPDATE`, old.issue); err != nil {
		t.Fatal(err)
	}
	if err := stream.s.TickWorkspaces(ctx, scope...); err != nil {
		t.Fatal(err)
	}
	if old.pending(t) != 1 || stream.pending(t) != 0 {
		t.Fatalf("setup: old pending %d, stream pending %d", old.pending(t), stream.pending(t))
	}
	if err := holder.Rollback(ctx); err != nil { // no lock from here on
		t.Fatal(err)
	}
	delivered := -1
	for tick := 0; tick < 12 && delivered < 0; tick++ {
		if tick > 0 && spacing > 0 {
			time.Sleep(spacing)
		}
		arriveClones(t, stream, arrivals)
		if err := stream.s.TickWorkspaces(ctx, scope...); err != nil {
			t.Fatal(err)
		}
		if old.pending(t) == 0 {
			delivered = tick
		}
	}
	if delivered < 0 || delivered >= scopedRevisitTicks {
		t.Fatalf("with %d arrivals per tick the released older input was delivered at tick %d (want within %d ticks, 0 = first)", arrivals, delivered, scopedRevisitTicks)
	}
	t.Logf("%d arrivals per tick: older input delivered at tick %d", arrivals, delivered)
	old.wantOutcomes(t, "delivered")
}

// An older input whose issue has become free is revisited even when new inputs
// arrive at, or above, the drain's per-pass budget (49, 50 and 60 per tick): the
// sweep is bounded, so it does not depend on the newer tail emptying.
func TestScopedDrainReleasedInputIsRevisitedUnderSteadyArrivals(t *testing.T) {
	for _, arrivals := range []int{49, 50, 60, 120} {
		t.Run(fmt.Sprint(arrivals), func(t *testing.T) { revisitUnderArrivals(t, arrivals, 0) })
	}
}

// The same at a scheduler-like spacing between ticks (scaled down: the sweep has
// no timer, so spacing cannot change the outcome), including a late tick.
func TestScopedDrainReleasedInputIsRevisitedAtSpacedTicks(t *testing.T) {
	if testing.Short() {
		t.Skip("spaced ticks")
	}
	revisitUnderArrivals(t, 50, 1500*time.Millisecond)
}

// A busy replay (receipt rows another writer holds, issue rows free) keeps the
// fairness position: successive passes advance and reach healthy work while the
// receipt locks are held, and a pass never spends more than a bounded time
// waiting for locks.
func TestScopedDrainReceiptLockContentionKeepsTheScanPosition(t *testing.T) {
	ctx := context.Background()
	var contended []drainKit
	var scope []pgtype.UUID
	for range 50 {
		k := newDrainKit(t)
		k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
		k.comment(t, "seed receipt")
		k.drain(t)
		arriveClones(t, k, 1)
		contended = append(contended, k)
		scope = append(scope, parseTestUUID(t, k.f.WorkspaceID))
	}
	healthy := newDrainKit(t)
	healthy.defineRoot(t, "workspace", healthy.f.WorkspaceID, scopedRuleA, "comment.created")
	healthy.comment(t, "healthy behind contended receipts")
	holder, err := healthy.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE workspace_id=ANY($1)) FOR UPDATE`, scope); err != nil {
		t.Fatal(err)
	}
	scope = append(scope, parseTestUUID(t, healthy.f.WorkspaceID))

	pass, err := healthy.s.drainOneScopedIssue(ctx, scope, time.Now(), scopedDrainBatch, []pgtype.UUID{}, time.Time{}, pgtype.UUID{Valid: true}, time.Now().Add(time.Hour), pgtype.UUID{Bytes: [16]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, Valid: true})
	if !scopedLockBusy(err) || !pass.busy.Valid {
		t.Fatalf("expected real replay contention: %+v %v", pass, err)
	}
	if pass.at.IsZero() || !pass.pos.Valid {
		t.Fatal("receipt contention discarded the claimed input's scan position")
	}
	delivered, longest := -1, time.Duration(0)
	for p := range 8 {
		started := time.Now()
		if err := healthy.s.DrainScopedEvents(ctx, scope...); err != nil {
			t.Fatalf("pass %d: contention must not surface as an error: %v", p, err)
		}
		if d := time.Since(started); d > longest {
			longest = d
		}
		if healthy.pending(t) == 0 && delivered < 0 {
			delivered = p
		}
	}
	if delivered < 0 {
		t.Fatal("50 receipt-contended inputs starved healthy work across eight passes while the receipt locks were held")
	}
	if longest > scopedDrainWaitBudget+time.Second {
		t.Fatalf("a pass took %v waiting for locks, bound is %v", longest, scopedDrainWaitBudget)
	}
	t.Logf("healthy delivered at pass %d with the receipt locks held; longest pass %v", delivered, longest)
	for _, k := range contended {
		if k.pending(t) != 1 {
			t.Fatal("a contended input changed disposition")
		}
	}
	holder.Rollback(ctx)
	for range 6 {
		if err := healthy.s.DrainScopedEvents(ctx, scope...); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range contended {
		if k.pending(t) != 0 {
			t.Fatal("released receipt inputs were not retried")
		}
	}
}

// A nil skip list must not make the claim match nothing.
func TestScopedClaimWithNilSkipListClaimsInputs(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.comment(t, "claimable")
	rows, err := k.s.Tasks.Queries.ClaimWakeupScopedEvents(context.Background(), db.ClaimWakeupScopedEventsParams{
		Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, Oldest: pgtype.Timestamptz{Time: time.Now().Add(-scopedEventRetention), Valid: true},
		WorkspaceIds: []pgtype.UUID{parseTestUUID(t, k.f.WorkspaceID)}, AfterAt: pgtype.Timestamptz{Valid: true}, AfterID: pgtype.UUID{Valid: true},
		UntilAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}, UntilID: pgtype.UUID{Bytes: [16]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, Valid: true},
		BatchSize: 10,
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim with a nil skip list = %d rows (err %v), want 1", len(rows), err)
	}
}
