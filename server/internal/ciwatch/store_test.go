package ciwatch

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Needs a migrated database: DATABASE_URL=... go test ./internal/ciwatch -run Store
func TestStoreIsIdempotentAndKeepsOnlyRedactedText(t *testing.T) {
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

	raw := "boom\nclone https://u:s3cretPass@h/x.git\n" + ghToken + "\n-----BEGIN " + "RSA PRIVATE KEY-----\n" + pemBody + "\n"
	gh := &fakeGH{jobsByPage: [][]Job{{failedJob(1, "build", t0)}}, logs: map[int64]func(context.Context) io.Reader{1: text(raw)}}
	c, _ := newCollector(gh, &fakeBudget{}, fakeRuns{})
	in := baseInput()
	in.Run.WorkspaceID = uuid.NewString()
	got, _ := c.Handle(ctx, in)

	q := db.New(pool)
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM ci_failure_event WHERE workspace_id = $1", in.Run.WorkspaceID) })
	if first, err := Store(ctx, q, got.Event); err != nil || !first {
		t.Fatalf("first insert: %v %v", first, err)
	}
	if again, err := Store(ctx, q, got.Event); err != nil || again {
		t.Fatalf("redelivery must be a no-op: %v %v", again, err)
	}
	var row string
	err = pool.QueryRow(ctx, "SELECT row_to_json(e)::text FROM ci_failure_event e WHERE workspace_id = $1", in.Run.WorkspaceID).Scan(&row)
	if err != nil {
		t.Fatal(err)
	}
	noLeak(t, row, pemBody, "s3cretPass", ghToken, "BEGIN RSA")
	var n int
	pool.QueryRow(ctx, "SELECT count(*) FROM ci_failure_event WHERE workspace_id = $1 AND evidence = 'full' AND failed_job_count = 1", in.Run.WorkspaceID).Scan(&n)
	if n != 1 {
		t.Errorf("evidence columns not stored, rows=%d", n)
	}
}
