package ghrun

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	testOwner = "cheese-work"
	testRepo  = "multica"
)

var testBase = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

type fixture struct {
	pool        *pgxpool.Pool
	workspaceID pgtype.UUID
}

func newFixture(test *testing.T) fixture {
	test.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		test.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	workspaceID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	test.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM github_workflow_run WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(ctx, `DELETE FROM github_workflow_job WHERE workspace_id = $1`, workspaceID)
	})
	return fixture{pool: pool, workspaceID: workspaceID}
}

func runEvent(runID int64, attempt int32, rank int32, status string, at time.Duration) RunEvent {
	return RunEvent{
		InstallationID: 1, RepoOwner: testOwner, RepoName: testRepo, RunID: runID, RunAttempt: attempt,
		StatusRank: rank, Status: status, GhUpdatedAt: testBase.Add(at),
		WorkflowPath: ".github/workflows/cd-deploy.yml", WorkflowName: "CD deploy", DisplayTitle: "deploy",
		HeadSHA: "59819ba7", HeadBranch: "main", Event: "push", HTMLURL: "https://example.test/run",
	}
}

func jobEvent(runID int64, attempt int32, jobID int64, phase int32) JobEvent {
	return JobEvent{
		InstallationID: 1, RepoOwner: testOwner, RepoName: testRepo, RunID: runID, RunAttempt: attempt,
		HeadSHA: "59819ba7", JobID: jobID, JobName: "deploy", Phase: phase,
	}
}

func (f fixture) run(test *testing.T, runID int64) db.GithubWorkflowRun {
	test.Helper()
	run, err := db.New(f.pool).GetGitHubWorkflowRun(context.Background(), db.GetGitHubWorkflowRunParams{
		WorkspaceID: f.workspaceID, RepoOwner: testOwner, RepoName: testRepo, RunID: runID,
	})
	if err != nil {
		test.Fatalf("run %d: %v", runID, err)
	}
	return run
}

func (f fixture) jobPhase(test *testing.T, runID int64, attempt int32, jobID int64) int32 {
	test.Helper()
	jobs, err := db.New(f.pool).ListGitHubWorkflowJobsByRunAttempt(context.Background(), db.ListGitHubWorkflowJobsByRunAttemptParams{
		WorkspaceID: f.workspaceID, RepoOwner: testOwner, RepoName: testRepo, RunID: runID, RunAttempt: attempt,
	})
	if err != nil {
		test.Fatal(err)
	}
	for _, job := range jobs {
		if job.JobID == jobID {
			return job.Phase
		}
	}
	return 0
}

func (f fixture) applyRun(test *testing.T, event RunEvent) {
	test.Helper()
	if err := ApplyRunEvent(context.Background(), f.pool, f.workspaceID, event); err != nil {
		test.Fatal(err)
	}
}

func (f fixture) applyJob(test *testing.T, event JobEvent) {
	test.Helper()
	if err := ApplyJobEvent(context.Background(), f.pool, f.workspaceID, event); err != nil {
		test.Fatal(err)
	}
}

// Test 1: reversed job delivery.
func TestReversedJobDelivery(test *testing.T) {
	f := newFixture(test)
	f.applyRun(test, runEvent(1, 1, RankInProgress, "in_progress", 0))
	f.applyJob(test, jobEvent(1, 1, 10, PhaseInProgress))
	f.applyJob(test, jobEvent(1, 1, 10, PhaseWaiting))
	if got := f.jobPhase(test, 1, 1, 10); got != PhaseInProgress {
		test.Fatalf("phase after in_progress then waiting = %d, want %d", got, PhaseInProgress)
	}
	if got := f.run(test, 1).GateGeneration; got != 0 {
		test.Fatalf("gate_generation = %d, want 0", got)
	}
	f.applyJob(test, jobEvent(1, 1, 11, PhaseCompleted))
	f.applyJob(test, jobEvent(1, 1, 11, PhaseInProgress))
	if got := f.jobPhase(test, 1, 1, 11); got != PhaseCompleted {
		test.Fatalf("phase after completed then in_progress = %d, want %d", got, PhaseCompleted)
	}
}

// Test 2: job-first hydration.
func TestJobFirstHydration(test *testing.T) {
	f := newFixture(test)
	f.applyJob(test, jobEvent(2, 1, 20, PhaseWaiting))
	stub := f.run(test, 2)
	if stub.StatusRank != RankStub || stub.GateGeneration != 1 || stub.HeadSha != "59819ba7" {
		test.Fatalf("stub = rank %d gate %d sha %q", stub.StatusRank, stub.GateGeneration, stub.HeadSha)
	}
	f.applyRun(test, runEvent(2, 1, RankRequested, "requested", 0))
	hydrated := f.run(test, 2)
	if hydrated.WorkflowPath != ".github/workflows/cd-deploy.yml" || hydrated.StatusRank != RankRequested {
		test.Fatalf("hydrated = path %q rank %d", hydrated.WorkflowPath, hydrated.StatusRank)
	}
	if hydrated.GateGeneration != 1 || f.jobPhase(test, 2, 1, 20) != PhaseWaiting {
		test.Fatalf("hydration lost gate %d or job phase %d", hydrated.GateGeneration, f.jobPhase(test, 2, 1, 20))
	}
	f.applyRun(test, runEvent(2, 1, RankCompleted, "completed", 2*time.Minute))
	f.applyRun(test, runEvent(2, 1, RankInProgress, "in_progress", 3*time.Minute))
	if got := f.run(test, 2); got.StatusRank != RankCompleted || got.Status != "completed" {
		test.Fatalf("late in_progress regressed completed run: rank %d status %q", got.StatusRank, got.Status)
	}
}

// Test 3: concurrent jobs.
func TestConcurrentJobsKeepGeneration(test *testing.T) {
	f := newFixture(test)
	f.applyRun(test, runEvent(3, 1, RankInProgress, "in_progress", 0))
	f.applyJob(test, jobEvent(3, 1, 30, PhaseWaiting))
	f.applyJob(test, jobEvent(3, 1, 31, PhaseWaiting))
	f.applyJob(test, jobEvent(3, 1, 30, PhaseInProgress))
	f.applyJob(test, jobEvent(3, 1, 31, PhaseWaiting))
	if got := f.run(test, 3).GateGeneration; got != 2 {
		test.Fatalf("gate_generation = %d, want 2", got)
	}
}

// Test 4: attempt rollover via run event.
func TestAttemptRolloverViaRunEvent(test *testing.T) {
	f := newFixture(test)
	f.applyRun(test, runEvent(4, 1, RankCompleted, "completed", 0))
	f.applyJob(test, jobEvent(4, 1, 40, PhaseWaiting))
	f.applyRun(test, runEvent(4, 2, RankRequested, "requested", time.Minute))
	rolled := f.run(test, 4)
	if rolled.RunAttempt != 2 || rolled.StatusRank != RankRequested || rolled.GateGeneration != 0 {
		test.Fatalf("rolled = attempt %d rank %d gate %d", rolled.RunAttempt, rolled.StatusRank, rolled.GateGeneration)
	}
	f.applyRun(test, runEvent(4, 1, RankCompleted, "completed", 2*time.Minute))
	if got := f.run(test, 4); got.RunAttempt != 2 || got.StatusRank != RankRequested {
		test.Fatalf("late attempt-1 run event changed the run: attempt %d rank %d", got.RunAttempt, got.StatusRank)
	}
	f.applyJob(test, jobEvent(4, 1, 41, PhaseWaiting))
	if got := f.run(test, 4).GateGeneration; got != 0 {
		test.Fatalf("late attempt-1 waiting incremented gate_generation to %d", got)
	}
	if got := f.jobPhase(test, 4, 1, 41); got != PhaseWaiting {
		test.Fatalf("late attempt-1 job row not stored: phase %d", got)
	}
}

// Test 5: job-led attempt raise.
func TestJobLedAttemptRaise(test *testing.T) {
	f := newFixture(test)
	event := runEvent(5, 1, RankCompleted, "completed", 0)
	event.Conclusion = "success"
	f.applyRun(test, event)
	f.applyJob(test, jobEvent(5, 2, 50, PhaseWaiting))
	raised := f.run(test, 5)
	if raised.RunAttempt != 2 || raised.StatusRank != RankStub || raised.GateGeneration != 1 {
		test.Fatalf("raised = attempt %d rank %d gate %d", raised.RunAttempt, raised.StatusRank, raised.GateGeneration)
	}
	if raised.Status != "" || raised.Conclusion != "" || raised.GhUpdatedAt.Valid {
		test.Fatalf("status fields not reset: %q %q %v", raised.Status, raised.Conclusion, raised.GhUpdatedAt)
	}
	if raised.WorkflowPath != ".github/workflows/cd-deploy.yml" || raised.HeadSha != "59819ba7" || raised.HtmlUrl == "" {
		test.Fatalf("metadata not kept: %+v", raised)
	}
}

// Test 6: late waiting after completion.
func TestLateWaitingAfterCompletion(test *testing.T) {
	f := newFixture(test)
	f.applyRun(test, runEvent(6, 1, RankCompleted, "completed", 0))
	f.applyJob(test, jobEvent(6, 1, 60, PhaseWaiting))
	if got := f.run(test, 6).GateGeneration; got != 0 {
		test.Fatalf("waiting after same-attempt completion incremented gate_generation to %d", got)
	}
	if got := f.jobPhase(test, 6, 1, 60); got != 0 {
		test.Fatalf("rejected waiting job was stored with phase %d", got)
	}
}

// Test 7: durable gate counter.
func TestDurableGateCounter(test *testing.T) {
	f := newFixture(test)
	f.applyRun(test, runEvent(7, 1, RankInProgress, "in_progress", 0))
	f.applyJob(test, jobEvent(7, 1, 70, PhaseWaiting))
	f.applyJob(test, jobEvent(7, 1, 70, PhaseInProgress))
	f.applyJob(test, jobEvent(7, 1, 71, PhaseWaiting))
	if got := f.run(test, 7).GateGeneration; got != 2 {
		test.Fatalf("gate_generation = %d, want 2", got)
	}
	f.applyJob(test, jobEvent(7, 1, 71, PhaseCompleted))
	if got := f.run(test, 7).GateGeneration; got != 2 {
		test.Fatalf("gate_generation dropped to %d after the gate closed", got)
	}
}

// Test 8: two transactions deliver waiting events at the same time.
func TestConcurrentWaitingDeliveries(test *testing.T) {
	f := newFixture(test)
	var group sync.WaitGroup
	errs := make(chan error, 2)
	for _, jobID := range []int64{80, 81} {
		group.Add(1)
		go func() {
			defer group.Done()
			errs <- ApplyJobEvent(context.Background(), f.pool, f.workspaceID, jobEvent(8, 1, jobID, PhaseWaiting))
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			test.Fatal(err)
		}
	}
	if got := f.run(test, 8).GateGeneration; got != 2 {
		test.Fatalf("gate_generation = %d, want exactly 2", got)
	}
}

// Same-attempt run events: ties and NULL timestamps.
func TestSameAttemptStatusOrdering(test *testing.T) {
	f := newFixture(test)
	f.applyJob(test, jobEvent(9, 1, 90, PhaseWaiting)) // stub, gh_updated_at NULL
	f.applyRun(test, runEvent(9, 1, RankInProgress, "in_progress", time.Minute))
	f.applyRun(test, runEvent(9, 1, RankRequested, "requested", 2*time.Minute)) // lower rank, newer time
	if got := f.run(test, 9); got.StatusRank != RankInProgress {
		test.Fatalf("lower rank overwrote status: rank %d", got.StatusRank)
	}
	newer := runEvent(9, 1, RankInProgress, "in_progress_b", 3*time.Minute)
	f.applyRun(test, newer)
	older := runEvent(9, 1, RankInProgress, "in_progress_a", 30*time.Second)
	f.applyRun(test, older)
	if got := f.run(test, 9); got.Status != "in_progress_b" {
		test.Fatalf("older equal-rank event overwrote status: %q", got.Status)
	}
}
