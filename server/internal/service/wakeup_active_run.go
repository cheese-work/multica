package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Active-run behavior (CHE-1082 L8). When an external fact reaches a built-in
// rule while its target agent is running on the issue, the rule either
// suppresses it (the legacy behavior of the PR rules, and what any rule that
// does not say gets) or defers it: the fact stays pending, the running prompt
// is never edited, and the next pass that finds the target idle makes one
// follow-up attempt through the usual gates (rate, aggregate, fire limit).
// Nothing selects defer implicitly; the plan's default of defer for new custom
// rules lives with custom-rule execution, not here.
const (
	wakeupActiveRunSuppress = "suppress"
	wakeupActiveRunDefer    = "defer"

	wakeupOutcomeDeferred     = "deferred_active_run"
	wakeupOutcomeDeferExpired = "defer_expired"
)

// wakeupDeferRetention bounds how long a running target may hold a fact back.
// Past it the fact is dropped with a visible outcome: retention is accounting,
// not a promise of eventual delivery.
const wakeupDeferRetention = 24 * time.Hour

func validWakeupActiveRun(v string) bool {
	return v == wakeupActiveRunSuppress || v == wakeupActiveRunDefer
}

// defersActiveRun reports an explicit, resolved defer choice.
func (b *builtinWakeup) defersActiveRun() bool {
	return b != nil && b.Eff.Config.ActiveRun.Set && b.Eff.Config.ActiveRun.Value == wakeupActiveRunDefer
}

// deferForActiveRun holds a deferring rule's pending facts back while its
// target runs. Facts older than the retention window expire visibly; facts held
// for the first time are noted once, and everything else stays pending
// untouched. Only a run that includes the evidence, or the self-actor rule
// (decided before this point), ever consumes a fact.
func deferForActiveRun(ctx context.Context, q *db.Queries, w db.IssueWakeup, target wakeTarget, receipts []db.IssueWakeupReceipt, now time.Time,
	note func(string, map[string]any) error) error {
	var expired, held []db.IssueWakeupReceipt
	for _, r := range receipts {
		if r.CreatedAt.Valid && now.Sub(r.CreatedAt.Time) > wakeupDeferRetention {
			expired = append(expired, r)
		} else {
			held = append(held, r)
		}
	}
	record := func(outcome string, rs []db.IssueWakeupReceipt) error {
		facts := systemWakeupFacts(w, rs)
		facts["target_type"], facts["target_id"] = target.Type, util.UUIDToString(target.ID)
		facts["outcome"] = outcome
		return note(wakeupActivityTriggered, facts)
	}
	if len(expired) > 0 {
		if err := q.ConsumeWakeupReceipts(ctx, db.ConsumeWakeupReceiptsParams{Ids: receiptIDs(expired), TaskID: pgtype.UUID{}}); err != nil {
			return err
		}
		if err := record(wakeupOutcomeDeferExpired, expired); err != nil {
			return err
		}
	}
	if len(held) == 0 {
		return nil
	}
	fresh, err := q.MarkWakeupReceiptsDeferred(ctx, receiptIDs(held))
	if err != nil || len(fresh) == 0 {
		return err
	}
	return record(wakeupOutcomeDeferred, fresh)
}
