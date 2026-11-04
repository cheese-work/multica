package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dispatch"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The queue door for Triage (MUL-7189 §2.3).
//
// The rule is narrower than "an issue in Triage runs nothing": Triage has no
// EXECUTOR. Nothing may derive one from the issue — the assignee, a prefilled
// owner, a squad leader, an automatic retry — because that is precisely what
// the triager is still deciding. Prefilling an owner is how a run starts today,
// and the triager must record its reasoning as a comment, so without this the
// triager would dispatch the very owner it was only proposing.
//
// A person naming an agent by hand is a different act. It is a conversation
// with that agent, its cost is one the person chose, and no triager action
// produces it, so it keeps working — the same shape as backlog, which parks
// assignment but lets comments through, one notch tighter because in Triage the
// assignee is only a proposal.
//
// The check cannot live at the call sites: several paths enqueue without
// looking at status at all, and dispatchIssueRun discards the enqueue error
// entirely, so a missed call site would fail silently. It sits at the last
// point every issue-linked task passes through — immediately before the INSERT
// — and the upstream short-circuits exist only to make previews and dispatch
// reasons correct.
//
// Exactly two enqueue paths are exempt, and both are Triage's own:
//
//   - the Triage run itself, the one run an issue in Triage is allowed;
//   - accept, which moves the issue out of Triage in the same transaction that
//     enqueues the execution run, so it passes this check anyway.
//
// Both belong to sub-issues that are not built yet.

// RunOrigin tells the queue door where a run came from, which is the whole of
// what Triage decides on.
//
// OriginDerived is the zero value on purpose: a caller that says nothing gets
// the strict answer, so a new enqueue path cannot leak into Triage by omission.
type RunOrigin int

const (
	// OriginDerived: the executor was worked out from the issue — its assignee,
	// its squad's leader, the parent of a finished sub-issue, a retry of an
	// earlier run. Refused in Triage.
	OriginDerived RunOrigin = iota
	// OriginNamed: a person wrote this agent's name — an @mention, or a reply
	// to something it said. Allowed in Triage.
	OriginNamed
	// OriginBudgetRecovery: the run the exhaustion move itself enqueues for the
	// blocked step's handler (CHE-2297 L3). Admitted at zero like a named run.
	OriginBudgetRecovery
)

// ErrIssueInTriage is returned by a derived enqueue path when the issue is
// waiting in Triage. Handlers render it as 403 issue_in_triage; background
// callers log it and drop the trigger.
var ErrIssueInTriage = errors.New("the issue is in triage, so nothing derived from it runs")

// ErrRerunSourceIsTriage is returned when a manual rerun names a triage run as
// the task to repeat. Handlers render it as 400: the request is well-formed and
// the issue may well be runnable — it is the target that is not an execution
// run. Redoing triage is re-triage.
var ErrRerunSourceIsTriage = errors.New("the source task is a triage run, which is redone by re-triaging the issue rather than by rerunning it")

// ErrRecoveryCaused is returned for any run a budget recovery run's own request
// would start: its assignment, rerun, @mention or reply. The recovery reserve
// buys one run; what that run causes buys nothing (CHE-2297 L3).
var ErrRecoveryCaused = errors.New("a budget recovery run cannot start another run")

// guardIssueRunAllowed refuses a DERIVED enqueue for an issue in Triage or one
// whose workflow run budget is spent (guardRunBudget). q is
// the caller's own query handle so a transaction-scoped enqueue reads the
// status its own transaction wrote — the deferred-channel path creates the
// issue and its task together, and an uncommitted Triage issue must still be
// seen.
//
// target names the agent and trigger of the run, for the run budget.
//
// A named run is never refused: it skips the Triage read and the budget check,
// and only takes the budget lock and window credit for its debit.
//
// A missing issue is not in Triage: the enqueue proceeds and fails (or not) on
// its own terms. A read error refuses, because a run that cannot be shown to be
// allowed must not start.
func guardIssueRunAllowed(ctx context.Context, q *db.Queries, issueID pgtype.UUID, origin RunOrigin, target RunTarget) error {
	_, err := guardIssueRunGate(ctx, q, issueID, origin, target)
	return err
}

// guardIssueRunGate is guardIssueRunAllowed for an enqueue that inserts the
// task row itself: it also returns the review gate to apply to that insert.
func guardIssueRunGate(ctx context.Context, q *db.Queries, issueID pgtype.UUID, origin RunOrigin, target RunTarget) (ReviewGate, error) {
	if err := RequireActiveIssueProject(ctx, q, issueID); err != nil {
		return ReviewGate{}, err
	}
	// Before the OriginNamed exit: a person naming an agent does not start a
	// second runner next to one that is not proven stopped.
	if issueID.Valid {
		if err := StopGate(ctx, q, issueID); err != nil {
			return ReviewGate{}, err
		}
	}
	if !issueID.Valid {
		return ReviewGate{}, nil
	}
	if origin != OriginBudgetRecovery && dispatch.BudgetRecoveryCaller(ctx) {
		return ReviewGate{}, ErrRecoveryCaused
	}
	if origin == OriginNamed || origin == OriginBudgetRecovery {
		// Never refused, but the lock and the window credit are taken here so
		// the named run's debit lands in the same window the others read.
		if _, _, _, err := openRunBudget(ctx, q, issueID, pgtype.UUID{}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return ReviewGate{}, fmt.Errorf("run budget: %w", err)
		}
		return ReviewGate{}, nil
	}
	state, err := q.GetIssueTriageState(ctx, issueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewGate{}, nil
		}
		return ReviewGate{}, fmt.Errorf("triage guard: load issue triage state: %w", err)
	}
	if state.Valid {
		return ReviewGate{}, ErrIssueInTriage
	}
	return guardRunBudget(ctx, q, issueID, target)
}

// TriageContextType marks a task as a Triage run in its context JSONB
// (MUL-7189 §3.6). Triage runs carry no retry budget and no resumable session:
// a fresh triage of the current entry is always the right recovery, and a later
// execution run on the accepted issue must not inherit the triage conversation.
const TriageContextType = "triage"

// IsTriageTask reports whether a task is a Triage run. The marker lives in the
// task's context rather than a column, so a task written before Triage existed
// reads as false.
//
// Exported because the daemon claim handler needs the same answer: it resolves
// a manual rerun's session from the named source task, which no query-level
// exclusion can reach.
func IsTriageTask(t db.AgentTaskQueue) bool {
	if len(t.Context) == 0 {
		return false
	}
	var payload struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(t.Context, &payload) == nil && payload.Type == TriageContextType
}
