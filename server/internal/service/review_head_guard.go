package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrReviewHeadAnswered is returned by an enqueue path when the reviewer agent
// already posted a verdict, BLOCKED or NOT-RUN for the PR head the run would
// review (CHE-4649). Handlers render it as 409 review_head_answered; background
// callers drop the trigger. The run is not a task row, so it costs no budget.
var ErrReviewHeadAnswered = errors.New("the agent already answered this PR head, so no review run starts")

// reviewGuardAgents is the reviewer set the guard applies to, read once from
// MULTICA_REVIEW_GUARD_AGENT_IDS (comma-separated agent ids). The server has no
// reviewer role, so the set is configuration; empty turns the guard off.
var reviewGuardAgents atomic.Pointer[map[string]bool]

func init() { SetReviewGuardAgents(strings.Split(os.Getenv("MULTICA_REVIEW_GUARD_AGENT_IDS"), ",")...) }

// SetReviewGuardAgents replaces the reviewer set (blank ids are ignored).
func SetReviewGuardAgents(ids ...string) {
	set := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			set[id] = true
		}
	}
	reviewGuardAgents.Store(&set)
}

// ReviewRunSkipped describes a refusal the hook records: the run that was not
// started, and the PR head the agent had already answered.
type ReviewRunSkipped struct {
	IssueID pgtype.UUID
	Target  RunTarget
	Head    string
}

var onReviewRunSkipped atomic.Pointer[func(context.Context, ReviewRunSkipped)]

// SetOnReviewRunSkipped registers the function that records a skipped run (nil
// clears it). The handler registers it: the refusal rolls back the enqueue
// transaction, so the record is written on its own connection, after it.
func SetOnReviewRunSkipped(fn func(context.Context, ReviewRunSkipped)) {
	if fn == nil {
		onReviewRunSkipped.Store(nil)
		return
	}
	onReviewRunSkipped.Store(&fn)
}

// guardReviewHeadAnswered refuses a reviewer run for a PR head the agent
// already answered.
//
// It acts only for a review run of an agent in the reviewer set, on an issue
// with exactly one linked open PR; every other run starts as before. A read
// error refuses nothing: the budget and stage guards still bound the run.
func guardReviewHeadAnswered(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) error {
	if !target.ReviewRun || !target.AgentID.Valid || !(*reviewGuardAgents.Load())[util.UUIDToString(target.AgentID)] {
		return nil
	}
	heads, err := q.ListIssueOpenPullRequestHeads(ctx, issueID)
	if err != nil || len(heads) != 1 || heads[0] == "" {
		return nil
	}
	answered, err := q.HasAgentVerdictForHead(ctx, db.HasAgentVerdictForHeadParams{IssueID: issueID, AgentID: target.AgentID, HeadSha: heads[0]})
	if err != nil || !answered {
		return nil
	}
	if hook := onReviewRunSkipped.Load(); hook != nil {
		event := ReviewRunSkipped{IssueID: issueID, Target: target, Head: heads[0]}
		goRunBudgetHook(func() { (*hook)(context.WithoutCancel(ctx), event) })
	}
	return ErrReviewHeadAnswered
}
