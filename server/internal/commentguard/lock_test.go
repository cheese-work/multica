package commentguard

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	testPool        *pgxpool.Pool
	testQueries     *db.Queries
	testDBFixture   *testutil.Fixture
	testWorkspaceID string
	testUserID      string
)

const commentguardTestWorkspaceSlug = "commentguard-lock-tests"

func TestMain(m *testing.M) {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		fmt.Printf("Skipping tests: could not connect to database: %v\n", err)
		os.Exit(0)
	}
	if err := pool.Ping(ctx); err != nil {
		fmt.Printf("Skipping tests: database not reachable: %v\n", err)
		pool.Close()
		os.Exit(0)
	}
	testPool = pool
	testQueries = db.New(pool)

	if err := cleanupCommentguardTestFixture(ctx, pool); err != nil {
		fmt.Printf("Failed to clean up commentguard test fixture: %v\n", err)
		pool.Close()
		os.Exit(1)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ($1, $2)
		RETURNING id
	`, "Commentguard Lock Test User", "commentguard-lock-test@multica.ai").Scan(&testUserID); err != nil {
		fmt.Printf("Failed to create test user: %v\n", err)
		pool.Close()
		os.Exit(1)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "Commentguard Lock Tests", commentguardTestWorkspaceSlug, "Temporary workspace for commentguard lock tests", "CGL").Scan(&testWorkspaceID); err != nil {
		fmt.Printf("Failed to create test workspace: %v\n", err)
		pool.Close()
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, testWorkspaceID, testUserID); err != nil {
		fmt.Printf("Failed to create test member: %v\n", err)
		pool.Close()
		os.Exit(1)
	}

	testDBFixture = testutil.New(pool, testWorkspaceID, testUserID)

	code := m.Run()
	if err := cleanupCommentguardTestFixture(context.Background(), pool); err != nil {
		fmt.Printf("Failed to clean up commentguard test fixture: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	pool.Close()
	os.Exit(code)
}

func cleanupCommentguardTestFixture(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, commentguardTestWorkspaceSlug); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, "commentguard-lock-test@multica.ai"); err != nil {
		return err
	}
	return nil
}
