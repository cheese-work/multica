package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-8749 S7a: claim poll statistics. Every claim poll on the agent, runtime
// and batch paths adds one count to an in-process histogram keyed by (path,
// outcome). Flush adds the counts to claim_poll_stats once a minute. A batch
// poll is one poll however many agents it tried, so the numbers answer "how
// long does a daemon wait for a claim answer", not "how many claimTask calls
// ran". Polls are measured in every queue order mode.

// Poll outcomes. claimed: at least one task returned. empty: nothing to claim.
// refused: candidates were visible but every attempt was refused (capacity,
// provider hold, fence). error: the poll failed. rank_walk_exhausted: the
// project-rank walk tried its row budget without a claim; the ordering slice
// (S4b) reports it, the base claim path never does.
const (
	ClaimPollOutcomeClaimed           = "claimed"
	ClaimPollOutcomeEmpty             = "empty"
	ClaimPollOutcomeRefused           = "refused"
	ClaimPollOutcomeError             = "error"
	ClaimPollOutcomeRankWalkExhausted = "rank_walk_exhausted"
)

// ClaimPollBucketBoundsMs are inclusive upper bounds in milliseconds: 1, 2, 5,
// then every 10 ms up to 1000. The rollout kill rule is "claim poll p95 up 50 ms
// or more", so a bucket must be much narrower than 50 ms; 10 ms wide buckets
// put a 50 ms shift five buckets apart. A poll slower than the last bound lands
// in a final overflow slot, so a bucket slice has len(ClaimPollBucketBoundsMs)+1
// entries. The server writes the slice as is; readers must use the same bounds.
var ClaimPollBucketBoundsMs = claimPollBucketBounds()

func claimPollBucketBounds() []float64 {
	bounds := []float64{1, 2, 5}
	for ms := 10.0; ms <= 1000; ms += 10 {
		bounds = append(bounds, ms)
	}
	return bounds
}

// ClaimPollPercentileMs estimates the q quantile (0 < q <= 1) of a bucket
// slice as the upper bound of the bucket holding it, so it never reads below
// the true value. This is the conservative rule for the rollback comparison:
// when the quantile falls in the overflow slot the poll has no upper bound and
// the result is +Inf, which trips a "p95 up 50 ms" check instead of hiding. No
// polls reads as 0.
func ClaimPollPercentileMs(buckets []int64, q float64) float64 {
	var total int64
	for _, n := range buckets {
		total += n
	}
	if total == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(total)))
	if rank < 1 {
		rank = 1
	}
	var seen int64
	for i, n := range buckets {
		seen += n
		if seen >= rank {
			if i >= len(ClaimPollBucketBoundsMs) {
				return math.Inf(1)
			}
			return ClaimPollBucketBoundsMs[i]
		}
	}
	return math.Inf(1)
}

// ClaimPollP95Rose is the rollback check "claim poll p95 rose by deltaMs or
// more", safe against overflow. A plain subtraction of two +Inf p95 values is
// NaN, and NaN >= deltaMs is false, so the check would pass silently. Rules:
//   - current p95 overflows (above the last bound): true. Either it rose, or it
//     is indeterminate (baseline overflows too); both mean "roll back or look".
//   - baseline overflows and current is finite: false; current is at most the
//     last bound, which is below the baseline.
//   - no polls in either window: false; nothing to compare.
//   - otherwise: current - baseline >= deltaMs.
func ClaimPollP95Rose(baseline, current []int64, deltaMs float64) bool {
	if sumBuckets(baseline) == 0 || sumBuckets(current) == 0 {
		return false
	}
	cur := ClaimPollPercentileMs(current, 0.95)
	if math.IsInf(cur, 1) {
		return true
	}
	base := ClaimPollPercentileMs(baseline, 0.95)
	if math.IsInf(base, 1) {
		return false
	}
	return cur-base >= deltaMs
}

func sumBuckets(b []int64) int64 {
	var n int64
	for _, v := range b {
		n += v
	}
	return n
}

// ClaimPollFlushInterval is how often RunClaimPollStatsFlusher writes.
const ClaimPollFlushInterval = 60 * time.Second

type ClaimPollKey struct {
	Path    string
	Outcome string
}

type ClaimPollEntry struct {
	Count   int64
	SumMs   float64
	Buckets []int64
}

type ClaimPollStats struct {
	mu      sync.Mutex
	entries map[ClaimPollKey]*ClaimPollEntry
}

func NewClaimPollStats() *ClaimPollStats {
	return &ClaimPollStats{entries: map[ClaimPollKey]*ClaimPollEntry{}}
}

func (p *ClaimPollStats) Observe(path, outcome string, d time.Duration) {
	if p == nil {
		return
	}
	ms := float64(d) / float64(time.Millisecond)
	slot := len(ClaimPollBucketBoundsMs)
	for i, bound := range ClaimPollBucketBoundsMs {
		if ms <= bound {
			slot = i
			break
		}
	}
	key := ClaimPollKey{Path: normalizeClaimPath(path), Outcome: outcome}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[key]
	if e == nil {
		e = &ClaimPollEntry{Buckets: make([]int64, len(ClaimPollBucketBoundsMs)+1)}
		p.entries[key] = e
	}
	e.Count++
	e.SumMs += ms
	e.Buckets[slot]++
}

// Snapshot returns a copy of the counts held since the last successful flush.
func (p *ClaimPollStats) Snapshot() map[ClaimPollKey]ClaimPollEntry {
	out := map[ClaimPollKey]ClaimPollEntry{}
	if p == nil {
		return out
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.entries {
		out[k] = ClaimPollEntry{Count: e.Count, SumMs: e.SumMs, Buckets: append([]int64(nil), e.Buckets...)}
	}
	return out
}

// Flush adds the held counts to claim_poll_stats under the given minute and
// empties the histogram. On a write failure the unwritten counts go back into
// the histogram, so the next flush carries them.
func (p *ClaimPollStats) Flush(ctx context.Context, q *db.Queries, at time.Time) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	taken := p.entries
	p.entries = map[ClaimPollKey]*ClaimPollEntry{}
	p.mu.Unlock()

	minute := pgtype.Timestamptz{Time: at.UTC().Truncate(time.Minute), Valid: true}
	var firstErr error
	for key, e := range taken {
		if err := q.UpsertClaimPollStats(ctx, db.UpsertClaimPollStatsParams{
			Minute:       minute,
			Path:         key.Path,
			Outcome:      key.Outcome,
			Count:        e.Count,
			SumMs:        e.SumMs,
			BucketCounts: e.Buckets,
		}); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("flush claim poll stats %s/%s: %w", key.Path, key.Outcome, err)
			}
			p.merge(key, e)
		}
	}
	return firstErr
}

func (p *ClaimPollStats) merge(key ClaimPollKey, add *ClaimPollEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[key]
	if e == nil {
		p.entries[key] = add
		return
	}
	e.Count += add.Count
	e.SumMs += add.SumMs
	for i, n := range add.Buckets {
		e.Buckets[i] += n
	}
}

// RunClaimPollStatsFlusher flushes every ClaimPollFlushInterval until ctx ends,
// then once more so a clean shutdown loses nothing.
func RunClaimPollStatsFlusher(ctx context.Context, stats *ClaimPollStats, q *db.Queries) {
	ticker := time.NewTicker(ClaimPollFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := stats.Flush(flushCtx, q, time.Now()); err != nil {
				slog.Warn("claim poll stats: final flush failed", "error", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := stats.Flush(ctx, q, time.Now()); err != nil {
				slog.Warn("claim poll stats: flush failed", "error", err)
			}
		}
	}
}

// claimPollTally collects what the claimTask calls inside one poll saw, so the
// poll can be classified once. Not safe for concurrent use: one poll runs its
// claimTask calls in sequence.
type claimPollTally struct {
	refused int
}

// note records one finished claimTask call. no_tasks with no visible
// candidate is an empty answer; a claim, an error (the poll reports it) and
// nothing else is not a refusal.
func (t *claimPollTally) note(outcome string, scope claimScope) {
	if t == nil {
		return
	}
	switch {
	case outcome == claimOutcomeClaimed:
	case strings.HasPrefix(outcome, "error_"):
	case outcome == claimOutcomeNoTasks && scope.candidates == 0:
	default:
		t.refused++
	}
}

// observePoll classifies and records one finished poll.
func (s *TaskService) observePoll(path string, start time.Time, claimed int, err error, tally *claimPollTally) {
	if s.PollStats == nil {
		return
	}
	outcome := ClaimPollOutcomeEmpty
	switch {
	case claimed > 0:
		outcome = ClaimPollOutcomeClaimed
	case err != nil:
		outcome = ClaimPollOutcomeError
	case tally != nil && tally.refused > 0:
		outcome = ClaimPollOutcomeRefused
	}
	s.PollStats.Observe(path, outcome, time.Since(start))
}
