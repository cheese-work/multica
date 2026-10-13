package handler

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// CHE-1665: a conflict always wakes the owner, a draft that is only behind
// does not, and a BEHIND wake never hides a later conflict on the same pair.

type mergeabilityFixture struct {
	set     func(head, mergeable, merge string)
	refresh func()
	count   func() int
	finish  func()
	draft   func(bool)
	// legacy seeds a ledger entry and processed receipt the way the build
	// before CHE-1665 wrote them: one key for DIRTY and BEHIND alike.
	legacy func(head, merge string)
}

func newMergeabilityFixture(t *testing.T, installationID int64, prNumber int32, head string) mergeabilityFixture {
	t.Helper()
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1665 mergeability", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	pr := linkedPRForWakeup(t, fx, installationID, prNumber, head)
	setWorkspacePRWakeSetting(t, "github_enabled", true)

	var mu sync.Mutex
	current := ghsnapshot.PRSnapshot{HeadSHA: head, BaseRef: "main", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}
	refresh := startSnapshotRefresh(t, installationID, prNumber, func() ghsnapshot.PRSnapshot {
		mu.Lock()
		defer mu.Unlock()
		return current
	})
	return mergeabilityFixture{
		set: func(h, mergeable, merge string) {
			mu.Lock()
			defer mu.Unlock()
			if h != head {
				if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET head_sha=$2 WHERE id=$1`, pr.ID, h); err != nil {
					t.Fatal(err)
				}
				head = h
			}
			current.HeadSHA, current.Mergeable, current.MergeStateStatus = h, mergeable, merge
		},
		refresh: refresh,
		count:   func() int { return prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention") },
		finish: func() {
			testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, fx.child.ID)
		},
		legacy: func(h, merge string) {
			ctx := context.Background()
			var wakeupID string
			var revision int64
			if err := testPool.QueryRow(ctx, `SELECT id, revision FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_needs_attention'`, fx.child.ID).Scan(&wakeupID, &revision); err != nil {
				t.Fatalf("read attention wakeup: %v", err)
			}
			key := fmt.Sprintf("github:acme/widget#%d:mergeability:%s:main", prNumber, h)
			if _, err := testPool.Exec(ctx, `INSERT INTO issue_wakeup_pr_event(wakeup_id,event_key) VALUES($1,$2)`, wakeupID, key); err != nil {
				t.Fatalf("seed legacy ledger entry: %v", err)
			}
			if _, err := testPool.Exec(ctx, `INSERT INTO issue_wakeup_receipt(id,wakeup_id,revision,event_key,event_type,payload,processed_at)
				VALUES(gen_random_uuid(),$1,$2,$3,'pr.mergeability',jsonb_build_object('reason','mergeability','merge_state',$4::text,'head_sha',$5::text,'base_branch','main'),now())`,
				wakeupID, revision, key, merge, h); err != nil {
				t.Fatalf("seed legacy receipt: %v", err)
			}
		},
		draft: func(on bool) {
			state := "open"
			if on {
				state = "draft"
			}
			if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET state=$2 WHERE id=$1`, pr.ID, state); err != nil {
				t.Fatal(err)
			}
		},
	}
}

func TestPRMergeabilityDraftWakesOnConflictOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := newMergeabilityFixture(t, 1665101, 166501, "che1665-draft-a")
	fx.draft(true)

	fx.set("che1665-draft-a", "MERGEABLE", "BEHIND")
	fx.refresh()
	if got := fx.count(); got != 0 {
		t.Fatalf("BEHIND on a draft PR queued %d attention wakeups, want 0", got)
	}

	fx.set("che1665-draft-a", "CONFLICTING", "DIRTY")
	fx.refresh()
	if got := fx.count(); got != 1 {
		t.Fatalf("DIRTY on a draft PR queued %d attention wakeups, want 1", got)
	}
	fx.finish()

	// GitHub may report a conflicting draft as DRAFT; mergeable still says
	// CONFLICTING, which is the conflict.
	fx.set("che1665-draft-b", "CONFLICTING", "DRAFT")
	fx.refresh()
	if got := fx.count(); got != 2 {
		t.Fatalf("conflicting draft reported as DRAFT queued %d attention wakeups in total, want 2", got)
	}
	fx.finish()

	fx.draft(false)
	fx.set("che1665-draft-c", "MERGEABLE", "BEHIND")
	fx.refresh()
	if got := fx.count(); got != 3 {
		t.Fatalf("BEHIND on a ready PR queued %d attention wakeups in total, want 3", got)
	}
}

func TestPRMergeabilityBehindDoesNotSuppressLaterDirty(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := newMergeabilityFixture(t, 1665102, 166502, "che1665-pair")

	fx.set("che1665-pair", "MERGEABLE", "BEHIND")
	fx.refresh()
	if got := fx.count(); got != 1 {
		t.Fatalf("BEHIND on a ready PR queued %d attention wakeups, want 1", got)
	}
	fx.finish()

	fx.set("che1665-pair", "CONFLICTING", "DIRTY")
	fx.refresh()
	if got := fx.count(); got != 2 {
		t.Fatalf("DIRTY after BEHIND on the same head and base queued %d attention wakeups in total, want 2", got)
	}
}

// A ledger entry written before this deploy used one key for both states. A
// legacy BEHIND entry must not hide a later DIRTY on its pair; a legacy DIRTY
// entry still deduplicates DIRTY.
func TestPRMergeabilityLegacyBehindEntryDoesNotSuppressDirty(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := newMergeabilityFixture(t, 1665103, 166503, "che1665-legacy-seed")

	// The first wake creates the issue's attention wakeup the legacy rows hang off.
	fx.set("che1665-legacy-seed", "CONFLICTING", "DIRTY")
	fx.refresh()
	if got := fx.count(); got != 1 {
		t.Fatalf("seed DIRTY queued %d attention wakeups, want 1", got)
	}
	fx.finish()
	fx.legacy("che1665-legacy-behind", "BEHIND")
	fx.legacy("che1665-legacy-dirty", "DIRTY")

	fx.set("che1665-legacy-behind", "CONFLICTING", "DIRTY")
	fx.refresh()
	if got := fx.count(); got != 2 {
		t.Fatalf("DIRTY after a legacy BEHIND entry queued %d attention wakeups in total, want 2", got)
	}
	fx.finish()
	fx.refresh()
	if got := fx.count(); got != 2 {
		t.Fatalf("repeated DIRTY on the same pair woke again: %d attention wakeups in total, want 2", got)
	}

	fx.set("che1665-legacy-dirty", "CONFLICTING", "DIRTY")
	fx.refresh()
	if got := fx.count(); got != 2 {
		t.Fatalf("DIRTY after a legacy DIRTY entry woke again: %d attention wakeups in total, want 2", got)
	}
	fx.set("che1665-legacy-dirty", "MERGEABLE", "BEHIND")
	fx.refresh()
	if got := fx.count(); got != 2 {
		t.Fatalf("BEHIND after a legacy DIRTY entry woke: %d attention wakeups in total, want 2", got)
	}
}
