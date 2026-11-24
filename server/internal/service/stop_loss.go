package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The stop-loss (CHE-9340): an issue that started StopLossRuns runs, or spent
// StopLossTokens tokens, and has no open, draft or merged PR linked starts no
// further derived run. The refusal parks it in need_decision with one card; the
// auto-approver never releases it. Only an approved card does: its allowance
// raises both limits in proportion, so each release buys a fresh slice of the
// same size per run.
//
// Exclusions: an issue with a live PR (an open PR in review counts), and a
// `plan` coordinator that still waits on open children.
//
// Mode is the workspace setting settings.run_guard.stop_loss.mode: off | warn |
// block. Absent, it warns: the issue is listed and nothing is refused.

const (
	StopLossRuns   = 20
	StopLossTokens = 30_000_000
)

// ErrStopLoss is returned when the stop-loss refuses a run. It wraps
// ErrRunBudgetExhausted so every enqueue caller already treats it as a refusal.
var ErrStopLoss = fmt.Errorf("stop-loss: the issue spent its runs or tokens with no PR: %w", ErrRunBudgetExhausted)

// StopLossState is one reading of an issue against the stop-loss.
type StopLossState struct {
	Mode    string
	Tripped bool
	Runs    int64
	Tokens  int64
}

// StopLossModeFromSettings reads settings.run_guard.stop_loss.mode. Absent or
// unreadable it is warn, so a bad value never refuses a run.
func StopLossModeFromSettings(settings []byte) string {
	var doc struct {
		Guard struct {
			StopLoss struct {
				Mode string `json:"mode"`
			} `json:"stop_loss"`
		} `json:"run_guard"`
	}
	if json.Unmarshal(settings, &doc) != nil {
		return WeeklyRunGuardWarn
	}
	switch doc.Guard.StopLoss.Mode {
	case WeeklyRunGuardOff, WeeklyRunGuardBlock:
		return doc.Guard.StopLoss.Mode
	}
	return WeeklyRunGuardWarn
}

// EvaluateStopLoss reads the issue against the stop-loss. It only reads.
func EvaluateStopLoss(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (StopLossState, error) {
	issue, err := q.GetIssue(ctx, issueID)
	if err != nil {
		return StopLossState{}, fmt.Errorf("stop-loss: load issue: %w", err)
	}
	ws, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return StopLossState{}, fmt.Errorf("stop-loss: load workspace: %w", err)
	}
	state := StopLossState{Mode: StopLossModeFromSettings(ws.Settings)}
	if state.Mode == WeeklyRunGuardOff {
		return state, nil
	}
	if state.Runs, err = q.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{IssueID: issueID, Since: pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true}}); err != nil {
		return StopLossState{}, fmt.Errorf("stop-loss: count runs: %w", err)
	}
	// Only an approved stop-loss card raises the limits; a weekly guard grant,
	// automatic or not, does not.
	released, err := q.SumStopLossAllowance(ctx, issueID)
	if err != nil {
		return StopLossState{}, fmt.Errorf("stop-loss: allowance: %w", err)
	}
	runLimit := StopLossRuns + released
	// The token sum is the dearer read: an issue under its run limit needs it to
	// pass, and a tripped one needs it for the card.
	if state.Runs < runLimit {
		if state.Tokens, err = q.SumIssueTokens(ctx, issueID); err != nil {
			return StopLossState{}, fmt.Errorf("stop-loss: sum tokens: %w", err)
		}
		if state.Tokens < StopLossTokens*runLimit/StopLossRuns {
			return state, nil
		}
	}
	live, err := q.CountIssueLivePullRequests(ctx, issueID)
	if err != nil {
		return StopLossState{}, fmt.Errorf("stop-loss: count PRs: %w", err)
	}
	if live > 0 {
		return state, nil
	}
	if typ, _ := issueworkflow.TitleType(issue.Title); typ == "plan" {
		open, err := q.CountOpenChildIssues(ctx, issueID)
		if err != nil {
			return StopLossState{}, fmt.Errorf("stop-loss: count children: %w", err)
		}
		if open > 0 {
			return state, nil
		}
	}
	if state.Runs >= runLimit {
		if state.Tokens, err = q.SumIssueTokens(ctx, issueID); err != nil {
			return StopLossState{}, fmt.Errorf("stop-loss: sum tokens: %w", err)
		}
	}
	state.Tripped = true
	return state, nil
}

// StopLossRelease is the run allowance an approved card adds to an issue: the
// limits after it exceed what the issue already used by grant runs, so the
// refused trigger resumes. An issue far over its limits (100 runs, 100 M
// tokens) gets more than grant, a fresh slice of grant runs on top of its use.
func StopLossRelease(ctx context.Context, q *db.Queries, issueID pgtype.UUID, grant int64) (int64, error) {
	runs, err := q.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{IssueID: issueID, Since: pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true}})
	if err != nil {
		return 0, fmt.Errorf("stop-loss: count runs: %w", err)
	}
	tokens, err := q.SumIssueTokens(ctx, issueID)
	if err != nil {
		return 0, fmt.Errorf("stop-loss: sum tokens: %w", err)
	}
	released, err := q.SumStopLossAllowance(ctx, issueID)
	if err != nil {
		return 0, fmt.Errorf("stop-loss: allowance: %w", err)
	}
	limit := StopLossRuns + released
	// The run limit whose token limit the issue's tokens just reach, rounded up.
	byTokens := (tokens*StopLossRuns + StopLossTokens - 1) / StopLossTokens
	return max(runs, byTokens, limit) + grant - limit, nil
}

// guardStopLoss refuses the run when the stop-loss blocks, and in warn mode
// reports the would-be refusal and lets the run through.
func guardStopLoss(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) error {
	state, err := EvaluateStopLoss(ctx, q, issueID)
	if err != nil {
		return err
	}
	if !state.Tripped {
		return nil
	}
	if hook := onWeeklyRunGuardTripped.Load(); hook != nil && !hookSuppressed(ctx) {
		event := WeeklyRunGuardTripped{IssueID: issueID, Runs: state.Runs, Tokens: state.Tokens, Target: target, StopLoss: true, Warn: state.Mode == WeeklyRunGuardWarn}
		if statuses, err := q.ListIssueLastRunStatuses(ctx, db.ListIssueLastRunStatusesParams{IssueID: issueID, Limit: 3}); err == nil {
			event.Outcomes = statuses
		}
		goRunBudgetHook(func() { (*hook)(context.WithoutCancel(ctx), event) })
	}
	if state.Mode == WeeklyRunGuardWarn {
		return nil
	}
	return ErrStopLoss
}
