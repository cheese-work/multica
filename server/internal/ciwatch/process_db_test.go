package ciwatch

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Needs a migrated database: DATABASE_URL=... go test ./internal/ciwatch -run ProcessDB
func TestProcessDBRecordsEachRunAttemptOnce(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var ws string
	slug := "ciobs-" + uuid.NewString()[:8]
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, issue_prefix) VALUES ('ci obs', $1, 'CIO') RETURNING id::text`, slug).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, "DELETE FROM system1_observation WHERE workspace_id = $1", ws)
		pool.Exec(ctx, "DELETE FROM ci_failure_event WHERE workspace_id = $1", ws)
		pool.Exec(ctx, "DELETE FROM workspace WHERE id = $1", ws)
	})

	q := db.New(pool)
	count := func(attempt int32) (n int) {
		pool.QueryRow(ctx, `SELECT count(*) FROM system1_observation WHERE workspace_id = $1 AND kind = 'ci_event' AND decision_source = 'rule' AND class = 'infra_rate_limit' AND choice_key = 'P1' AND inputs->>'signature_hash' <> '' AND $2::int = $2::int`, ws, attempt).Scan(&n)
		return
	}
	process := func(attempt int32) {
		c, _ := newCollector(failingRunGH(), &fakeBudget{}, fakeRuns{})
		in := baseInput()
		in.Run.WorkspaceID, in.Run.RunAttempt = ws, attempt
		p := Pipeline{Collector: c, Tx: PgxTx(pool), Observer: Observer{W: q}}
		if _, err := p.Process(ctx, in, History{}); err != nil {
			t.Fatal(err)
		}
	}
	process(1)
	process(1) // redelivery
	if n := count(1); n != 1 {
		t.Fatalf("after a redelivery: %d rows, want 1", n)
	}
	process(2) // a rerun is a new attempt
	if n := count(2); n != 2 {
		t.Fatalf("after attempt 2: %d rows, want 2", n)
	}
	// An observation lost after the event was stored is written on the retry.
	pool.Exec(ctx, "DELETE FROM system1_observation WHERE workspace_id = $1", ws)
	process(1)
	if n := count(1); n != 1 {
		t.Fatalf("after recovery: %d rows, want 1", n)
	}
}
