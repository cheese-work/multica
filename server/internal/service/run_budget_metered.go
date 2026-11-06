package service

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The metered-spend limit on agent-granted run credit (CHE-3251). Agent credit
// is admission control, not a hard cap: a run is admitted while the month's
// metered spend plus the runs in flight, plus this run at its price, stays at
// or under 80% of the limit. What one run really costs is known only after it
// reports usage, so a run can overshoot its price; the 20% margin absorbs it.

// ErrRunMeteredLimit refuses a run on agent-granted credit because the month's
// metered spend is at its ceiling. It is a run-budget refusal, so every caller
// that handles ErrRunBudgetExhausted handles it.
var ErrRunMeteredLimit = fmt.Errorf("%w: the month's metered spend is at its ceiling", ErrRunBudgetExhausted)

// MeteredGate is the process-wide admission line for runs on agent credit.
type MeteredGate struct {
	PerRunMicroUSD int64
	LimitMicroUSD  int64
}

var meteredGate atomic.Pointer[MeteredGate]

// SetMeteredGate installs the gate; nil removes it, and runs on agent credit
// are then refused.
func SetMeteredGate(g *MeteredGate) { meteredGate.Store(g) }

// MeteredCeiling is the line spend plus commitments may not pass: 80% of the limit.
func MeteredCeiling(limitMicroUSD int64) int64 { return limitMicroUSD * 4 / 5 }

// ICTMonth is the Indochina-time month holding now: [start, next).
func ICTMonth(now time.Time) (start, next time.Time) {
	ict := time.FixedZone("ICT", 7*3600)
	n := now.In(ict)
	start = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, ict)
	return start, start.AddDate(0, 1, 0)
}

// MeteredPosition is what the month has committed, in micro-USD.
type MeteredPosition struct {
	Spent    int64 // usage reported this month
	InFlight int64 // runs without usage yet, at the price of a run
	Credit   int64 // agent-granted credit not yet used, at the price of a run
}

// ReadMeteredPosition reads the position across the instance. exceptTask is a
// run being admitted, which must not count against itself (zero UUID: none).
func ReadMeteredPosition(ctx context.Context, q *db.Queries, now time.Time, perRun int64, exceptTask pgtype.UUID) (MeteredPosition, error) {
	start, next := ICTMonth(now)
	rows, err := q.SumMeteredUsageBetween(ctx, db.SumMeteredUsageBetweenParams{
		Since: pgtype.Timestamptz{Time: start, Valid: true}, Until: pgtype.Timestamptz{Time: next, Valid: true},
	})
	if err != nil {
		return MeteredPosition{}, fmt.Errorf("metered spend: usage: %w", err)
	}
	var p MeteredPosition
	for _, r := range rows {
		p.Spent += usageMicroUSD(r, perRun)
	}
	inFlight, err := q.CountRunsInFlightWithoutUsage(ctx, db.CountRunsInFlightWithoutUsageParams{
		Since: pgtype.Timestamptz{Time: start, Valid: true}, ExceptTaskID: exceptTask,
	})
	if err != nil {
		return MeteredPosition{}, fmt.Errorf("metered spend: in flight: %w", err)
	}
	credit, err := q.SumAgentCreditOutstanding(ctx)
	if err != nil {
		return MeteredPosition{}, fmt.Errorf("metered spend: credit: %w", err)
	}
	p.InFlight, p.Credit = inFlight*perRun, credit*perRun
	return p, nil
}

// usageMicroUSD prices a usage group: the provider's ticks (1e-10 USD) when it
// reported them, else the rate table, else the price of a run for each task.
func usageMicroUSD(r db.SumMeteredUsageBetweenRow, perRun int64) int64 {
	if r.Priced {
		return r.Ticks / 10_000
	}
	price, ok := metrics.PriceForModelAlias(r.Model)
	if !ok {
		return r.Tasks * perRun
	}
	// $/M tokens times tokens is micro-USD.
	cost := float64(r.InputTokens)*price.InputPerM + float64(r.OutputTokens)*price.OutputPerM +
		float64(r.CacheReadTokens)*price.CacheReadPerM + float64(r.CacheWriteTokens)*price.CacheWritePerM
	return int64(math.Ceil(cost))
}

// RecordAgentRunCredit records runs of credit an agent granted to the issue.
// eventKey is the audit row's id, so a replay is a no-op.
func RecordAgentRunCredit(ctx context.Context, q *db.Queries, issue db.Issue, eventKey pgtype.UUID, runs int) error {
	return q.InsertIssueAgentCreditEntry(ctx, db.InsertIssueAgentCreditEntryParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		EventKey: util.UUIDToString(eventKey), AgentDelta: int32(runs),
	})
}

// admitMeteredRun refuses the run being debited when it is paid by agent credit
// and the month's metered spend has no room for it. balance is the issue's
// balance before this run. Runs inside a person's allowance are never gated:
// runs use the allowance first, so the last `credit` runs of the balance are
// the agents'. A gate that is not installed, or a spend that cannot be read,
// refuses.
func admitMeteredRun(ctx context.Context, q *db.Queries, workspaceID, issueID, taskID pgtype.UUID, balance int64) error {
	credit, err := q.SumIssueAgentCredit(ctx, issueID)
	if err != nil {
		return fmt.Errorf("metered spend: issue credit: %w", err)
	}
	if credit <= 0 || balance > credit {
		return nil
	}
	gate := meteredGate.Load()
	if gate == nil {
		return ErrRunMeteredLimit
	}
	// One lock for the whole instance, after the issue's own: two runs racing
	// for the last room under the ceiling read one at a time.
	if err := q.LockAgentRunBudgetCap(ctx); err != nil {
		return fmt.Errorf("metered spend: lock: %w", err)
	}
	p, err := ReadMeteredPosition(ctx, q, time.Now(), gate.PerRunMicroUSD, taskID)
	if err != nil {
		return err
	}
	if p.Spent+p.InFlight+gate.PerRunMicroUSD > MeteredCeiling(gate.LimitMicroUSD) {
		return ErrRunMeteredLimit
	}
	// The run uses one run of the agents' credit up: a later raise or top-up
	// by a person is the person's.
	return q.InsertIssueAgentCreditEntry(ctx, db.InsertIssueAgentCreditEntryParams{
		ID: dbid.NewV7(), WorkspaceID: workspaceID, IssueID: issueID,
		EventKey: "run:" + util.UUIDToString(taskID), AgentDelta: -1,
	})
}
