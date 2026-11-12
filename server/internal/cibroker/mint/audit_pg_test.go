package mint

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPGAuditRecordsRowWithoutTokenColumn(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass('ci_broker_audit') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Skip("ci_broker_audit migration not applied")
	}
	const repoID = int64(990_000_001)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM ci_broker_audit WHERE repository_id=$1`, repoID)
	})

	err = NewPGAudit(pool).Record(context.Background(), Row{
		At: time.Now(), RepositoryID: repoID, Repository: "o/r", RunID: "1", RunAttempt: "1",
		Permissions: Permissions{"contents": "read"}, Result: "issued",
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_broker_audit WHERE repository_id=$1 AND permissions->>'contents'='read'`, repoID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, err = %v", n, err)
	}
	var cols int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM information_schema.columns WHERE table_name='ci_broker_audit' AND (column_name ILIKE '%token%' OR column_name ILIKE '%jwt%')`).Scan(&cols)
	if cols != 0 {
		t.Fatalf("audit table has %d token/jwt columns", cols)
	}
}
