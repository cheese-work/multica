package handler

import (
	"context"
	"log/slog"

	"github.com/multica-ai/multica/server/internal/intakehold"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// intakeHoldSkips (CHE-6911, E3) says whether a decision delivery must dispatch
// nothing because the issue is held. Call it inside the delivery transaction,
// under the issue lock: it reads the facts there. Mode block returns true and
// writes one intake_hold_delivery_skipped line; mode warn returns false and
// writes one intake_hold_warn line; mode off, no entry or an issue the rule
// does not hold returns false and writes nothing. site names the caller.
func intakeHoldSkips(ctx context.Context, q db.DBTX, issueID, site string) (bool, error) {
	loaded, err := intakehold.Load(ctx, q, issueID)
	if err != nil {
		return false, err
	}
	verdict, applies := intakehold.Evaluate(loaded.Issue, loaded.Entry, loaded.Now)
	if !applies || !verdict.Held {
		return false, nil
	}
	if loaded.Entry.Mode == intakehold.ModeWarn {
		slog.Warn("intake_hold_warn", "issue_id", issueID, "site", site, "source", loaded.Entry.Source, "reason", verdict.Reason)
		return false, nil
	}
	slog.Info("intake_hold_delivery_skipped", "issue_id", issueID, "site", site, "source", loaded.Entry.Source, "reason", verdict.Reason)
	return true, nil
}
