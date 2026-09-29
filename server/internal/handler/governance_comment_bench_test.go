package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/governance/receipt"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/jev"
)

// BenchmarkGovernanceComment* measure default-off (outer flag off) overhead of
// the governance observation hook on comment create/update against a matched
// bypassed-hook control (Handler.GovernanceReceipts == nil). Go's ns/op is not
// a p95, so every measured request is appended to $GOVERNANCE_PERF_SAMPLES as
// one JSONL line; scripts/governance-perf-summarize.mjs computes the verdict.
//
// Replay (from server/, DATABASE_URL set to a migrated disposable database):
//
//	GOVERNANCE_PERF_SAMPLES=$PWD/../perf-samples.jsonl go test ./internal/handler \
//	  -run '^$' -bench '^BenchmarkGovernanceComment' -benchtime=1000x -count=5 -timeout=30m
//	node ../scripts/governance-perf-summarize.mjs ../perf-samples.jsonl
//
// Mode order alternates per invocation (A/B then B/A) and each invocation
// warms 100 requests first. No -race for performance samples. Fixture seed:
// none (fixed content strings; the issue row is created fresh per benchmark).

const governancePerfWarmup = 100

type tracedStats struct {
	stmts, govStmts, dbNanos atomic.Int64
}

type tracedStart struct{ t time.Time }
type tracedKey struct{}

func (s *tracedStats) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	s.stmts.Add(1)
	if strings.Contains(d.SQL, "governance_") {
		s.govStmts.Add(1)
	}
	return context.WithValue(ctx, tracedKey{}, tracedStart{time.Now()})
}

func (s *tracedStats) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if st, ok := ctx.Value(tracedKey{}).(tracedStart); ok {
		s.dbNanos.Add(int64(time.Since(st.t)))
	}
}

type perfCountingProvider struct{ calls atomic.Int64 }

func (p *perfCountingProvider) Evaluate(context.Context, jev.Request) (*jev.Response, error) {
	p.calls.Add(1)
	return nil, fmt.Errorf("provider must not run when the outer flag is off")
}

type perfSample struct {
	Bench, Mode                 string
	Batch, Seq                  int
	ClientNs, DBNs              int64
	DBStmts, GovStmts, Provider int64
}

func newTracedHandler(b *testing.B, stats *tracedStats) *Handler {
	b.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		b.Skip("DATABASE_URL not set")
	}
	cfg.ConnConfig.Tracer = stats
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(pool.Close)
	h := New(db.New(pool), pool, realtime.NewHub(), events.New(), service.NewEmailService(), nil, nil, analytics.NoopClient{}, Config{AllowSignup: true})
	sp := featureflag.NewStaticProvider()
	sp.LoadRules(map[string]featureflag.Rule{featureflags.JevReceipts: {Default: false}})
	h.FeatureFlags = featureflag.NewService(sp)
	return h
}

func runGovernanceCommentBench(b *testing.B, name string, op func(h *Handler, issueID, commentID string, n int) int) {
	// Go runs every benchmark once with b.N == 1 before the measured run; that
	// pass would emit a 1-sample batch and consume an A/B slot, so do nothing.
	if b.N == 1 {
		return
	}
	if testPool == nil {
		b.Skip("database not available")
	}
	out := os.Getenv("GOVERNANCE_PERF_SAMPLES")
	if out == "" {
		b.Skip("set GOVERNANCE_PERF_SAMPLES to retain raw per-request samples")
	}
	file, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()

	stats := &tracedStats{}
	h := newTracedHandler(b, stats)
	provider := &perfCountingProvider{}
	treatment := &receipt.Observer{
		Provider: provider, Store: h.Queries,
		BudgetAdmission: handlerTestBudgetAdmission{},
		BudgetPolicy:    &governance.DeploymentBudgetPolicy{Version: "perf", Provider: "jev", Model: "perf"},
	}
	var issueID, commentID string
	if err := testPool.QueryRow(context.Background(), `
		WITH n AS (UPDATE workspace SET issue_counter = issue_counter + 1 WHERE id = $1 RETURNING issue_counter)
		INSERT INTO issue (workspace_id, creator_type, creator_id, title, number, last_activity_at)
		SELECT $1, 'member', $2, 'governance perf', issue_counter, now() FROM n RETURNING id`,
		testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		b.Fatal(err)
	}
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
		VALUES ($1, $2, 'member', $3, 'seed') RETURNING id`, testWorkspaceID, issueID, testUserID).Scan(&commentID); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)
	})

	modes := []string{"bypassed", "flag_off_hook"}
	batchCounter, _ := governancePerfBatch.LoadOrStore(name, new(atomic.Int64))
	batch := int(batchCounter.(*atomic.Int64).Add(1)) - 1
	if batch%2 == 1 {
		modes[0], modes[1] = modes[1], modes[0]
	}
	enc := json.NewEncoder(file)
	seq := 0
	for _, mode := range modes {
		if mode == "bypassed" {
			h.GovernanceReceipts = nil
		} else {
			h.GovernanceReceipts = treatment
		}
		for i := 0; i < governancePerfWarmup; i++ {
			op(h, issueID, commentID, seq)
			seq++
		}
		// One batch = b.N measured requests per mode, run inline (no b.Run:
		// sub-benchmarks would run the parent once and defeat -count batches).
		for i := 0; i < b.N; i++ {
			s0, g0, d0, p0 := stats.stmts.Load(), stats.govStmts.Load(), stats.dbNanos.Load(), provider.calls.Load()
			start := time.Now()
			op(h, issueID, commentID, seq)
			client := time.Since(start)
			_ = enc.Encode(perfSample{Bench: name, Mode: mode, Batch: batch, Seq: i,
				ClientNs: client.Nanoseconds(), DBNs: stats.dbNanos.Load() - d0,
				DBStmts: stats.stmts.Load() - s0, GovStmts: stats.govStmts.Load() - g0, Provider: provider.calls.Load() - p0})
			seq++
		}
	}
}

// governancePerfBatch counts invocations per benchmark name so A/B order
// alternates independently for create and update.
var governancePerfBatch sync.Map

func BenchmarkGovernanceCommentCreate(b *testing.B) {
	runGovernanceCommentBench(b, "create", func(h *Handler, issueID, _ string, n int) int {
		w := httptest.NewRecorder()
		r := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments",
			map[string]any{"content": fmt.Sprintf("perf comment %d", n)}), "id", issueID)
		h.CreateComment(w, r)
		if w.Code != http.StatusCreated {
			b.Fatalf("CreateComment %d: %s", w.Code, w.Body.String())
		}
		return w.Code
	})
}

func BenchmarkGovernanceCommentUpdate(b *testing.B) {
	runGovernanceCommentBench(b, "update", func(h *Handler, _ string, commentID string, n int) int {
		w := httptest.NewRecorder()
		r := withURLParam(newRequest(http.MethodPut, "/api/comments/"+commentID,
			map[string]any{"content": fmt.Sprintf("perf edit %d", n)}), "commentId", commentID)
		h.UpdateComment(w, r)
		if w.Code != http.StatusOK {
			b.Fatalf("UpdateComment %d: %s", w.Code, w.Body.String())
		}
		return w.Code
	})
}
