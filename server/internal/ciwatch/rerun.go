package ciwatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/ghbudget"
)

// RunState is what GET /actions/runs/{id} says about a run.
type RunState struct {
	Attempt   int
	Completed bool
}

// RerunResponse is the answer to POST .../rerun-failed-jobs. A transport
// failure or timeout is an error, not a response.
type RerunResponse struct {
	Status  int
	Header  http.Header
	Message string
}

// RerunAPI is the installation-scoped GitHub surface of the P1 executor.
type RerunAPI interface {
	GetRun(ctx context.Context, inst int64, owner, repo string, runID int64) (RunState, http.Header, error)
	RerunFailedJobs(ctx context.Context, inst int64, owner, repo string, runID int64) (RerunResponse, error)
}

// NotSentError: the executor stopped before it sent anything. The ledger puts
// the row back to scheduled at DueAt (bounded by MaxAttempts).
type NotSentError struct {
	DueAt time.Time
	Err   error
}

func (e *NotSentError) Error() string { return "not sent: " + e.Err.Error() }
func (e *NotSentError) Unwrap() error { return e.Err }

// RerunDelay is how long a transient failure, or a rate limit with no usable
// reset, waits before the one rerun.
const RerunDelay = 2 * time.Minute

// rerunCost: one GET (run state) and one POST.
const rerunCost = 2

// Rerunner executes the P1 `rerun` action (CHE-4278). It sends at most one
// POST for an action; every other outcome is settled by reading run_attempt.
type Rerunner struct {
	API          RerunAPI
	Budget       *ghbudget.Ledger
	Installation func(ctx context.Context, a Action) (int64, error)
	Now          func() time.Time
}

func (r *Rerunner) notSent(err error) error {
	var de *ghbudget.DeferredError
	if errors.As(err, &de) {
		return &NotSentError{DueAt: de.DueAt, Err: err}
	}
	return &NotSentError{DueAt: r.Now().Add(RerunDelay), Err: err}
}

// What the POST answered. Stored on the row (post_outcome) before anything is
// done about it, so a restart can still route it (RerunFollowup).
const (
	OutcomePermission = "permission" // 403, not a rate limit
	OutcomeRateLimit  = "rate_limit" // 429, or 403 with a rate-limit signal
	OutcomeRejected   = "rejected"   // any other 4xx
	OutcomeServer     = "server"     // 5xx: the rerun may have landed
	OutcomeNoAnswer   = "no_answer"  // timeout or transport error: the rerun may have landed
	// "" (never stored): the worker died, or its write failed, after the POST.
	// The answer may have been a permission 403, so followup treats it as one.
)

// PostError is a POST that was sent and answered with something other than
// 2xx. The ledger stores Outcome and parks the row as ambiguous.
type PostError struct {
	Outcome string
	Err     error
}

func (e *PostError) Error() string { return e.Err.Error() }
func (e *PostError) Unwrap() error { return e.Err }

// Execute: everything before mark() is read-only and returns NotSentError; the
// POST happens once, after expected_attempt and post_started_at are stored.
// Any answer except 2xx is an error, so the ledger parks the row as ambiguous
// and reconcile reads the truth from run_attempt.
func (r *Rerunner) Execute(ctx context.Context, a Action, mark func(expectedAttempt int) error) (RemoteOutcome, error) {
	inst, err := r.Installation(ctx, a)
	if err != nil {
		return RemoteOutcome{}, r.notSent(err)
	}
	res, err := r.Budget.Reserve(ctx, inst, rerunCost)
	if err != nil {
		return RemoteOutcome{}, r.notSent(err)
	}
	st, hdr, err := r.API.GetRun(ctx, inst, a.RepoOwner, a.RepoName, a.OriginalRunID)
	// The GET went out, so the reservation is always settled with Complete: a
	// release would spend the shared installation budget without a debit.
	settle := func(h http.Header) { _ = res.Complete(context.WithoutCancel(ctx), h) }
	if err == nil && !st.Completed {
		err = errors.New("run is not completed")
	}
	if err == nil {
		err = res.Recheck(ctx)
	}
	if err != nil {
		settle(hdr)
		return RemoteOutcome{}, r.notSent(err)
	}
	if st.Attempt > 1 { // somebody already reran it
		settle(hdr)
		return RemoteOutcome{State: StateDone, Reason: ReasonManualRerun, DestinationID: strconv.Itoa(st.Attempt)}, nil
	}
	expected := st.Attempt + 1
	if err := mark(expected); err != nil {
		settle(hdr)
		return RemoteOutcome{}, r.notSent(err)
	}

	resp, err := r.API.RerunFailedJobs(ctx, inst, a.RepoOwner, a.RepoName, a.OriginalRunID)
	if err != nil { // timeout or transport error: the rerun may have landed
		settle(nil)
		return RemoteOutcome{}, &PostError{Outcome: OutcomeNoAnswer, Err: fmt.Errorf("rerun POST: %w", err)}
	}
	settle(resp.Header)
	if resp.Status >= 200 && resp.Status < 300 {
		return RemoteOutcome{State: StateDone, DestinationID: strconv.Itoa(expected)}, nil
	}
	outcome := OutcomeRejected
	switch {
	case resp.Status == http.StatusTooManyRequests || resp.Status == http.StatusForbidden && isRateLimited(resp):
		outcome = OutcomeRateLimit
		_, _ = r.Budget.RecordLimited(context.WithoutCancel(ctx), inst, resp.Header)
	case resp.Status == http.StatusForbidden:
		outcome = OutcomePermission
	case resp.Status >= 500:
		outcome = OutcomeServer
	}
	return RemoteOutcome{}, &PostError{Outcome: outcome, Err: fmt.Errorf("rerun POST HTTP %d: %s", resp.Status, resp.Message)}
}

func isRateLimited(resp RerunResponse) bool {
	return resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0" ||
		strings.Contains(strings.ToLower(resp.Message), "rate limit")
}

// Reconcile is read-only: the rerun landed when run_attempt reached the value
// stored before the POST. No stored value means the POST was never sent.
func (r *Rerunner) Reconcile(ctx context.Context, a Action) (RemoteOutcome, bool, error) {
	if a.ExpectedAttempt == nil {
		return RemoteOutcome{}, false, nil
	}
	inst, err := r.Installation(ctx, a)
	if err != nil {
		return RemoteOutcome{}, false, err
	}
	res, err := r.Budget.Reserve(ctx, inst, 1)
	if err != nil {
		return RemoteOutcome{}, false, err
	}
	st, hdr, err := r.API.GetRun(ctx, inst, a.RepoOwner, a.RepoName, a.OriginalRunID)
	_ = res.Complete(ctx, hdr)
	if err != nil || st.Attempt < *a.ExpectedAttempt {
		return RemoteOutcome{}, false, err
	}
	return RemoteOutcome{State: StateDone, DestinationID: strconv.Itoa(st.Attempt)}, true, nil
}

// DueForRerun: a rate limit waits for the reset, everything else (and a reset
// that is past or unknown) for two minutes.
func DueForRerun(class Class, now, resetAt time.Time) time.Time {
	if class == ClassRateLimit && resetAt.After(now) {
		return resetAt
	}
	return now.Add(RerunDelay)
}

// EscalationAfterRerun: where a second failure of the same original run goes.
func EscalationAfterRerun(class Class) (protocol string, ok bool) {
	switch class {
	case ClassRateLimit:
		return "P4", true
	case ClassTransient:
		return "P2", true
	}
	return "", false
}

// Route is where a failed run goes after P1 looked at it.
type Route struct {
	Protocol string // P1 (rerun scheduled), P2 or P4 (escalated)
	Action   Action
}

// ScheduleP1 claims the one rerun of the original run, or escalates. Delivery
// of the same failure can repeat, so only one thing changes anything:
//   - run_attempt 1: the first call schedules the rerun; a repeat does nothing.
//   - run_attempt > 1: the rerun already happened and failed again. The first
//     call schedules the one P2/P4 action; a repeat does nothing.
//
// A zero Route means "nothing new to do".
func ScheduleP1(ctx context.Context, l *Ledger, run Run, class Class, signature string, resetAt, now time.Time) (Route, error) {
	escalate, ok := EscalationAfterRerun(class)
	if !ok {
		return Route{}, fmt.Errorf("class %q has no P1 rerun", class)
	}
	n := NewAction{WorkspaceID: run.WorkspaceID, RepoOwner: run.RepoOwner, RepoName: run.RepoName,
		OriginalRunID: run.RunID, Kind: ActionRerun, Protocol: "P1", SignatureHash: signature,
		DueAt: DueForRerun(class, now, resetAt), RunCompletedAt: now}
	if run.RunAttempt > 1 {
		n.Kind, n.Protocol, n.DueAt = escalationKind(escalate), escalate, now
		a, inserted, err := l.escalate(ctx, nil, n)
		if err != nil || !inserted {
			return Route{}, err
		}
		return Route{Protocol: escalate, Action: a}, nil
	}
	a, inserted, err := l.Schedule(ctx, n)
	if err != nil || !inserted {
		return Route{}, err
	}
	return Route{Protocol: "P1", Action: a}, nil
}
