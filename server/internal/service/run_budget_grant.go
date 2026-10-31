package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Progress grants and the superseded-head refund (CHE-2297 L2c). A grant adds
// RunBudgetGrantRuns runs and RunBudgetGrantTokens weighted tokens to the open
// window without touching its cap; a window takes at most RunBudgetGrantCap of
// them, and none once its exhaustion claim exists. Every function runs in the
// caller's transaction and takes the issue lock, so concurrent grants keep the
// cap.
const (
	RunBudgetGrantRuns   = 6
	RunBudgetGrantTokens = 15_000_000
	RunBudgetGrantCap    = 3

	RunBudgetKindStep    = "step"
	RunBudgetKindHead    = "head"
	RunBudgetKindStalePR = "stale_pr"
	RunBudgetKindRefund  = "refund"
)

type grantWindow struct {
	issue db.Issue
	usage *runBudgetUsage
	id    string
}

// openGrantWindow locks the issue and returns its open window, or false when
// the issue is not budgeted, the window is exhausted, or it already holds the
// cap of grants.
func openGrantWindow(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (grantWindow, bool, error) {
	issue, usage, _, err := openRunBudget(ctx, q, issueID, pgtype.UUID{})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && usage == nil) {
		return grantWindow{}, false, nil
	}
	if err != nil {
		return grantWindow{}, false, fmt.Errorf("run budget: grant window: %w", err)
	}
	w := grantWindow{issue: issue, usage: usage, id: runBudgetWindowID(usage.Since)}
	exhausted, err := q.HasRunBudgetNotice(ctx, db.HasRunBudgetNoticeParams{IssueID: issue.ID, WindowID: w.id, Kind: RunBudgetKindExhausted})
	if err != nil || exhausted {
		return grantWindow{}, false, err
	}
	granted, err := q.CountIssueRunBudgetGrants(ctx, db.CountIssueRunBudgetGrantsParams{IssueID: issue.ID, WindowID: w.id})
	if err != nil || granted >= RunBudgetGrantCap {
		return grantWindow{}, false, err
	}
	return w, true, nil
}

// grant writes one grant. Tokens are credited only once token accounting is
// active for the issue; a replayed key writes nothing.
func (w grantWindow) grant(ctx context.Context, q *db.Queries, kind, key string) error {
	var tokens int64
	if _, active, err := activationEpoch(ctx, q, w.issue.ID); err != nil {
		return err
	} else if active && w.usage.tokensStored() {
		tokens = RunBudgetGrantTokens
	}
	return q.InsertIssueRunBudgetGrant(ctx, db.InsertIssueRunBudgetGrantParams{
		ID: dbid.NewV7(), WorkspaceID: w.issue.WorkspaceID, IssueID: w.issue.ID,
		WindowID: w.id, Kind: kind, EventKey: key, RunsDelta: RunBudgetGrantRuns, TokensDelta: tokens,
	})
}

// GrantRunBudgetStep grants the first entry of a status in the window by an
// agent or a workflow edge. The status the window opened in, a status a person
// moved the issue into, and any status entered before count as visited and
// grant nothing. A recovery-caused move grants nothing.
func GrantRunBudgetStep(ctx context.Context, q *db.Queries, issue db.Issue, prevStatus string, recovery bool) error {
	if recovery {
		return nil
	}
	w, ok, err := openGrantWindow(ctx, q, issue.ID)
	if err != nil || !ok {
		return err
	}
	since := pgtype.Timestamptz{Time: w.usage.Since, Valid: true}
	opened, err := q.FirstStatusInWindow(ctx, db.FirstStatusInWindowParams{IssueID: w.issue.ID, Since: since})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("run budget: window status: %w", err)
	}
	if opened == "" {
		opened = prevStatus
	}
	status := w.issue.Status
	if status == opened {
		return nil
	}
	byPerson, err := q.HasMemberMoveToStatusSince(ctx, db.HasMemberMoveToStatusSinceParams{IssueID: w.issue.ID, Since: since, Status: status})
	if err != nil || byPerson {
		return err
	}
	return w.grant(ctx, q, RunBudgetKindStep, RunBudgetKindStep+":"+w.id+":"+status)
}

// GrantRunBudgetHead grants a PR head SHA this issue never saw.
func GrantRunBudgetHead(ctx context.Context, q *db.Queries, issueID, prID pgtype.UUID, sha string) error {
	w, ok, err := openGrantWindow(ctx, q, issueID)
	if err != nil || !ok {
		return err
	}
	return w.grant(ctx, q, RunBudgetKindHead, RunBudgetKindHead+":"+util.UUIDToString(prID)+":"+sha)
}

// RefundSupersededRun gives back the run a superseded_head cancellation spent:
// +1 run, no tokens, once per task. Only a run debited in the current window is
// refunded; an earlier window's debit is not this window's to give back.
func RefundSupersededRun(ctx context.Context, q *db.Queries, task db.AgentTaskQueue) error {
	if !task.IssueID.Valid || task.FailureReason.String != FailureReasonSupersededHead {
		return nil
	}
	issue, usage, _, err := openRunBudget(ctx, q, task.IssueID, pgtype.UUID{})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && usage == nil) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run budget: refund: %w", err)
	}
	windowID := runBudgetWindowID(usage.Since)
	taskKey := util.UUIDToString(task.ID)
	debited, err := q.HasIssueRunBudgetEntryInWindow(ctx, db.HasIssueRunBudgetEntryInWindowParams{
		IssueID: issue.ID, WindowID: windowID, Kind: runBudgetKindRun, EventKey: runBudgetKindRun + ":" + taskKey,
	})
	if err != nil || !debited {
		return err
	}
	return q.InsertIssueRunBudgetGrant(ctx, db.InsertIssueRunBudgetGrantParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		WindowID: windowID, Kind: RunBudgetKindRefund, EventKey: RunBudgetKindRefund + ":" + taskKey, RunsDelta: 1,
	})
}
