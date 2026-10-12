package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// maxLegacyBackfillPage bounds one backfill transaction.
const maxLegacyBackfillPage = 500

// LegacyIssueWakeupDefinition projects what a person changed on a platform
// rule's issue row into an issue-scope definition. The row stays the legacy
// source of truth; this is its definition-model view. An uncustomized row
// follows inheritance and projects nothing; an empty instruction means inherit,
// never "run with no instruction"; a pause (rate, loop, max fires, blocked
// runs) is execution state, so a paused row contributes no enabled override.
// It returns nil when nothing is left to say.
func LegacyIssueWakeupDefinition(w db.IssueWakeup) (*WakeupDefinition, error) {
	if !w.SystemRule.Valid || !w.CustomizedAt.Valid {
		return nil, nil
	}
	if _, ok := WakeupBuiltinBaseline(w.SystemRule.String); !ok {
		return nil, fmt.Errorf("wakeup %s: unknown system rule %q", uuidString(w.ID), w.SystemRule.String)
	}
	var p WakeupConfigPatch
	if !w.PausedReason.Valid && !w.DisabledAt.Valid {
		p.Enabled = wakeupField[bool]{Set: true, Value: w.Enabled}
	}
	if text := strings.TrimSpace(w.Instruction); text != "" {
		p.Instruction = wakeupField[string]{Set: true, Value: text}
	}
	if !p.Enabled.Set && !p.Instruction.Set {
		return nil, nil
	}
	return &WakeupDefinition{
		Scope: WakeupScopeIssue, ScopeID: w.IssueID, Patch: p,
		Revision: wakeupContentRevision(boolFieldKey(p.Enabled), p.Instruction.Value),
	}, nil
}

// LegacyBackfillRequest selects one page of the legacy backfill. After is the
// cursor a previous page returned (invalid starts from the beginning);
// WorkspaceID, when valid, limits it to one workspace.
type LegacyBackfillRequest struct {
	After       pgtype.UUID
	WorkspaceID pgtype.UUID
	Limit       int32
}

// LegacyBackfillResult reports one page, or the sum of a run. Next is the
// cursor to resume from; Done is set when no rows were left after it.
type LegacyBackfillResult struct {
	Scanned, Inserted, Skipped int
	Next                       pgtype.UUID
	Done                       bool
}

// BackfillLegacyWakeupDefinitions materializes one page of customized legacy
// platform-rule rows as issue-scope definitions. Workspace values are not
// copied: they live on in the canonical settings aliases. It is idempotent
// and restartable, never replaces an existing definition, and only inserts
// definition rows: it creates no receipts, tasks or runtime rows and changes
// none. Nothing calls it yet; activating it belongs to the layer that opens
// definition writes.
func (s *IssueWakeupService) BackfillLegacyWakeupDefinitions(ctx context.Context, req LegacyBackfillRequest) (LegacyBackfillResult, error) {
	if req.Limit < 1 {
		return LegacyBackfillResult{}, fmt.Errorf("%w: backfill page limit must be at least 1", ErrWakeupInput)
	}
	limit := min(req.Limit, maxLegacyBackfillPage)
	after := req.After
	if !after.Valid {
		after = pgtype.UUID{Valid: true}
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return LegacyBackfillResult{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	rows, err := q.ListCustomizedSystemWakeupsAfter(ctx, db.ListCustomizedSystemWakeupsAfterParams{AfterID: after, WorkspaceID: req.WorkspaceID, PageLimit: limit})
	if err != nil {
		return LegacyBackfillResult{}, err
	}
	out := LegacyBackfillResult{Scanned: len(rows), Next: after, Done: len(rows) < int(limit)}
	for _, row := range rows {
		out.Next = row.ID
		def, err := LegacyIssueWakeupDefinition(row)
		if err != nil {
			return LegacyBackfillResult{}, err
		}
		if def == nil {
			out.Skipped++
			continue
		}
		config, err := MarshalWakeupConfigPatch(def.Patch)
		if err != nil {
			return LegacyBackfillResult{}, err
		}
		n, err := q.InsertWakeupDefinitionIfAbsent(ctx, db.InsertWakeupDefinitionIfAbsentParams{
			WorkspaceID: row.WorkspaceID, ScopeKind: string(WakeupScopeIssue), ScopeID: row.IssueID, RuleKey: row.SystemRule.String, Config: config,
		})
		if err != nil {
			return LegacyBackfillResult{}, err
		}
		out.Inserted += int(n)
	}
	return out, tx.Commit(ctx)
}

// RunLegacyWakeupBackfill pages through the whole backfill from the start. A
// page that fails leaves earlier pages committed; running it again is safe and
// skips what exists. workspaceID may be invalid for every workspace.
func (s *IssueWakeupService) RunLegacyWakeupBackfill(ctx context.Context, workspaceID pgtype.UUID, page int32) (LegacyBackfillResult, error) {
	var total LegacyBackfillResult
	req := LegacyBackfillRequest{WorkspaceID: workspaceID, Limit: page}
	for {
		res, err := s.BackfillLegacyWakeupDefinitions(ctx, req)
		if err != nil {
			return total, fmt.Errorf("legacy wakeup backfill (%d rows scanned): %w", total.Scanned, err)
		}
		total.Scanned += res.Scanned
		total.Inserted += res.Inserted
		total.Skipped += res.Skipped
		total.Next, total.Done = res.Next, res.Done
		if res.Done {
			return total, nil
		}
		req.After = res.Next
	}
}
