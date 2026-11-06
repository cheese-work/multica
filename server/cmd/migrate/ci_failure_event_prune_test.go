package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ci_failure_event keeps 30 days, like github_workflow_run: the prune removes
// rows created before the cutoff and nothing newer.
func TestDeleteExpiredCIFailureEvents(t *testing.T) {
	pool := openTestPool(t)
	ctx := t.Context()
	ws := uuid.New()
	t.Cleanup(func() { _, _ = pool.Exec(t.Context(), `DELETE FROM ci_failure_event WHERE workspace_id = $1`, ws) })
	now := time.Now()
	for run, age := range map[int64]time.Duration{1: 31 * 24 * time.Hour, 2: 29 * 24 * time.Hour} {
		if _, err := pool.Exec(ctx, `INSERT INTO ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt, created_at) VALUES ($1, 'o', 'r', $2, 1, $3)`, ws, run, now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := pgtype.Timestamptz{Time: now.Add(-30 * 24 * time.Hour), Valid: true}
	if _, err := db.New(pool).DeleteExpiredCIFailureEvents(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	var left []int64
	rows, err := pool.Query(ctx, `SELECT run_id FROM ci_failure_event WHERE workspace_id = $1`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if len(left) != 1 || left[0] != 2 {
		t.Fatalf("rows left after prune = %v, want only run 2", left)
	}
}
