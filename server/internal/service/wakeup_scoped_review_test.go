package service

import (
	"context"
	"testing"
	"time"

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
		t.Fatalf("busy pending = %d, want the locked input left for the next pass", busy.pending(t))
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
