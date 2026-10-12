package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// countingDBTX counts every statement sent through it.
type countingDBTX struct {
	inner db.DBTX
	n     atomic.Int64
}

func (c *countingDBTX) Exec(ctx context.Context, q string, a ...interface{}) (pgconn.CommandTag, error) {
	c.n.Add(1)
	return c.inner.Exec(ctx, q, a...)
}

func (c *countingDBTX) Query(ctx context.Context, q string, a ...interface{}) (pgx.Rows, error) {
	c.n.Add(1)
	return c.inner.Query(ctx, q, a...)
}

func (c *countingDBTX) QueryRow(ctx context.Context, q string, a ...interface{}) pgx.Row {
	c.n.Add(1)
	return c.inner.QueryRow(ctx, q, a...)
}

// withCountingQueries swaps the handler's Queries for one that counts
// statements; workspace-role checks use the same Queries, so the count is the
// route's full statement cost.
func withCountingQueries(t *testing.T) *countingDBTX {
	t.Helper()
	counter := &countingDBTX{inner: testPool}
	previous := testHandler.Queries
	testHandler.Queries = db.New(counter)
	t.Cleanup(func() { testHandler.Queries = previous })
	return counter
}

// seedGovernanceCases adds n cases (distinct created_at) to the fixture workspace.
func seedGovernanceCases(t *testing.T, workspaceID string, n int) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_case (
    id, workspace_id, subject_type, subject_id, subject_revision, rule_id,
    generation, material_fingerprint, state, authority_lineage, trigger_aliases,
    evidence_digest, rule_revision, activation_revision, config_revision,
    budget_root_id, frozen_strategy, reason, created_at
)
SELECT gen_random_uuid(), $1, 'issue', gen_random_uuid(), 1, gen_random_uuid(),
       0, 'perf-fp-' || i, 'captured', '[]'::jsonb, '[]'::jsonb,
       'perf-fp-' || i, 'r', 'a', 'c', gen_random_uuid(), '[]'::jsonb, 'perf',
       now() + i * interval '1 second'
FROM generate_series(1, $2::int) AS s(i)`, workspaceID, n); err != nil {
		t.Fatalf("seed governance cases: %v", err)
	}
}

type caseListPage struct {
	Cases []struct {
		ID string `json:"id"`
	} `json:"cases"`
	NextCursor *struct {
		CreatedAt string `json:"created_at"`
		ID        string `json:"id"`
	} `json:"next_cursor"`
}

// Case list: page sizes 1/25/50, limit+1 fetch, no skips/dupes, statement count
// per route fixed regardless of returned item count, over-limit rejected.
func TestGovernanceCaseAuditPageBoundsAndQueryCount(t *testing.T) {
	requireProvenanceDB(t)
	governanceCaseAuditFlag(t, true)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	fixture := newGovernanceCaseAuditFixture(t, suffix, "perf-sentinel-"+suffix) // 1 case
	const total = 120
	seedGovernanceCases(t, fixture.workspaceID, total-1)
	counter := withCountingQueries(t)

	var baseline int64 = -1
	for _, limit := range []int{1, 25, 50} {
		seen := map[string]bool{}
		var cursor *struct {
			CreatedAt string `json:"created_at"`
			ID        string `json:"id"`
		}
		for pageNo := 0; ; pageNo++ {
			q := url.Values{"limit": {fmt.Sprint(limit)}}
			if cursor != nil {
				q.Set("before_created_at", cursor.CreatedAt)
				q.Set("before_id", cursor.ID)
			}
			before := counter.n.Load()
			w := testutil.Call(t, testHandler.ListGovernanceCaseAudit,
				governanceCaseAuditRequest(http.MethodGet, "/api/governance/cases?"+q.Encode(), fixture)).Want(http.StatusOK)
			stmts := counter.n.Load() - before
			var page caseListPage
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Cases) > limit {
				t.Fatalf("limit=%d returned %d items", limit, len(page.Cases))
			}
			if baseline < 0 {
				baseline = stmts
			}
			if stmts != baseline {
				t.Fatalf("limit=%d page=%d: %d statements, want fixed %d (no per-item queries)", limit, pageNo, stmts, baseline)
			}
			for _, c := range page.Cases {
				if seen[c.ID] {
					t.Fatalf("limit=%d: duplicate case %s", limit, c.ID)
				}
				seen[c.ID] = true
			}
			if page.NextCursor == nil {
				break
			}
			cursor = page.NextCursor
		}
		if len(seen) != total {
			t.Fatalf("limit=%d: paged %d cases, want %d (skip)", limit, len(seen), total)
		}
	}
	t.Logf("case list statements per request = %d (fixed, independent of page size 1/25/50)", baseline)

	for _, bad := range []string{"0", "51", "-1", "x"} {
		testutil.Call(t, testHandler.ListGovernanceCaseAudit,
			governanceCaseAuditRequest(http.MethodGet, "/api/governance/cases?limit="+bad, fixture)).Want(http.StatusBadRequest)
	}
}

// Detail route: statement count is fixed (case + 3 page queries + auth)
// regardless of page size, i.e. no per-attempt/per-transition loop.
func TestGovernanceCaseAuditDetailQueryCountIsFixed(t *testing.T) {
	requireProvenanceDB(t)
	governanceCaseAuditFlag(t, true)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	fixture := newGovernanceCaseAuditFixture(t, suffix, "perf-detail-"+suffix)
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_attempt (workspace_id, case_id, ordinal, kind, input_digest, attempt_fence, created_at)
SELECT $1, $2, i, 'jev', 'perf-attempt-' || i, gen_random_uuid(), now() + i * interval '1 second'
FROM generate_series(1, 60) AS s(i)`, fixture.workspaceID, fixture.caseID); err != nil {
		t.Fatal(err)
	}
	counter := withCountingQueries(t)
	var baseline int64 = -1
	for _, limit := range []int{1, 25, 50} {
		before := counter.n.Load()
		w := testutil.Call(t, testHandler.GetGovernanceCaseAudit,
			withURLParam(governanceCaseAuditRequest(http.MethodGet,
				fmt.Sprintf("/api/governance/cases/%s?limit=%d", fixture.caseID, limit), fixture), "caseId", fixture.caseID)).Want(http.StatusOK)
		stmts := counter.n.Load() - before
		var body struct {
			Attempts []json.RawMessage `json:"attempts"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if len(body.Attempts) != min(limit, 60) {
			t.Fatalf("limit=%d attempts=%d", limit, len(body.Attempts))
		}
		if baseline < 0 {
			baseline = stmts
		}
		if stmts != baseline {
			t.Fatalf("limit=%d: %d statements, want fixed %d", limit, stmts, baseline)
		}
	}
	t.Logf("case detail statements per request = %d (fixed)", baseline)
}
