package migrations

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Table first, then one concurrent index per file (repo rule).
var githubWorkflowMigrations = []string{
	"711_github_workflow_run",
	"712_github_workflow_run_identity_index",
	"713_github_workflow_run_head_sha_index",
	"714_github_workflow_run_updated_at_index",
	"715_github_workflow_job",
	"716_github_workflow_job_identity_index",
	"717_github_workflow_job_run_attempt_index",
	"718_github_workflow_job_updated_at_index",
}

func TestGitHubWorkflowSchema(test *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		test.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "github_workflow_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		test.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			test.Error(err)
		}
	})
	applyAll := func() {
		test.Helper()
		for _, migration := range githubWorkflowMigrations {
			applyMigrationFile(test, ctx, pool, migration+".up.sql")
		}
	}
	applyAll()

	workspaceID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	otherWorkspaceID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	insertRun := func(workspace pgtype.UUID, runID int64) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO github_workflow_run (workspace_id, installation_id, repo_owner, repo_name, run_id, run_attempt, head_sha)
			VALUES ($1, 1, 'cheese-work', 'multica', $2, 1, 'abc')`, workspace, runID)
		return err
	}
	insertJob := func(workspace pgtype.UUID, runID, jobID int64, attempt int32) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO github_workflow_job (workspace_id, repo_owner, repo_name, run_id, run_attempt, job_id, job_name, phase)
			VALUES ($1, 'cheese-work', 'multica', $2, $3, $4, 'deploy', 1)`, workspace, runID, attempt, jobID)
		return err
	}
	isUniqueViolation := func(err error) bool {
		var pgError *pgconn.PgError
		return errors.As(err, &pgError) && pgError.Code == "23505"
	}

	test.Run("run_unique_identity", func(test *testing.T) {
		if err := insertRun(workspaceID, 100); err != nil {
			test.Fatal(err)
		}
		if err := insertRun(workspaceID, 100); !isUniqueViolation(err) {
			test.Fatalf("duplicate run: got %v, want unique violation", err)
		}
		if err := insertRun(otherWorkspaceID, 100); err != nil {
			test.Fatalf("same run in another workspace must be allowed: %v", err)
		}
	})
	test.Run("job_unique_identity", func(test *testing.T) {
		if err := insertJob(workspaceID, 100, 7, 1); err != nil {
			test.Fatal(err)
		}
		if err := insertJob(workspaceID, 100, 7, 1); !isUniqueViolation(err) {
			test.Fatalf("duplicate job: got %v, want unique violation", err)
		}
	})
	test.Run("run_defaults", func(test *testing.T) {
		queries := db.New(pool)
		run, err := queries.GetGitHubWorkflowRun(ctx, db.GetGitHubWorkflowRunParams{
			WorkspaceID: workspaceID, RepoOwner: "cheese-work", RepoName: "multica", RunID: 100,
		})
		if err != nil {
			test.Fatal(err)
		}
		if run.StatusRank != -1 || run.GateGeneration != 0 || run.GhUpdatedAt.Valid {
			test.Fatalf("unexpected defaults: %+v", run)
		}
	})
	test.Run("read_queries_scoped_by_workspace", func(test *testing.T) {
		queries := db.New(pool)
		if err := insertJob(workspaceID, 100, 8, 2); err != nil {
			test.Fatal(err)
		}
		runs, err := queries.ListGitHubWorkflowRunsByHeadSHA(ctx, db.ListGitHubWorkflowRunsByHeadSHAParams{
			WorkspaceID: workspaceID, RepoOwner: "cheese-work", RepoName: "multica", HeadSha: "abc",
		})
		if err != nil || len(runs) != 1 || runs[0].WorkspaceID != workspaceID {
			test.Fatalf("runs by head_sha: %v %+v", err, runs)
		}
		jobs, err := queries.ListGitHubWorkflowJobsByRunAttempt(ctx, db.ListGitHubWorkflowJobsByRunAttemptParams{
			WorkspaceID: workspaceID, RepoOwner: "cheese-work", RepoName: "multica", RunID: 100, RunAttempt: 1,
		})
		if err != nil || len(jobs) != 1 || jobs[0].JobID != 7 {
			test.Fatalf("jobs by run attempt: %v %+v", err, jobs)
		}
		jobs, err = queries.ListGitHubWorkflowJobsByRunAttempt(ctx, db.ListGitHubWorkflowJobsByRunAttemptParams{
			WorkspaceID: otherWorkspaceID, RepoOwner: "cheese-work", RepoName: "multica", RunID: 100, RunAttempt: 1,
		})
		if err != nil || len(jobs) != 0 {
			test.Fatalf("other workspace must see no jobs: %v %+v", err, jobs)
		}
		if _, err := queries.GetGitHubWorkflowRun(ctx, db.GetGitHubWorkflowRunParams{
			WorkspaceID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, RepoOwner: "cheese-work", RepoName: "multica", RunID: 100,
		}); !errors.Is(err, pgx.ErrNoRows) {
			test.Fatalf("unknown workspace: got %v, want no rows", err)
		}
	})
	test.Run("down_then_up", func(test *testing.T) {
		for index := len(githubWorkflowMigrations) - 1; index >= 0; index-- {
			applyMigrationFile(test, ctx, pool, githubWorkflowMigrations[index]+".down.sql")
		}
		var run, job *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass('github_workflow_run')::text, to_regclass('github_workflow_job')::text`).Scan(&run, &job); err != nil {
			test.Fatal(err)
		}
		if run != nil || job != nil {
			test.Fatalf("tables survive rollback: run=%v job=%v", run, job)
		}
		applyAll()
		var indexes int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND tablename IN ('github_workflow_run', 'github_workflow_job')`, schema).Scan(&indexes); err != nil {
			test.Fatal(err)
		}
		if indexes != 6 {
			test.Fatalf("indexes after re-apply = %d, want 6", indexes)
		}
	})
}
