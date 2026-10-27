// Package ghrun applies GitHub Actions workflow_run / workflow_job webhook
// events to the github_workflow_run / github_workflow_job mirror behind the
// `github_run` wakeup condition (CHE-2147).
//
// Deliveries arrive out of order and more than once, so every write is
// guarded. One transaction per workspace takes the run row lock first, which
// serializes all events for one run whichever event type arrives first.
package ghrun

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Run lifecycle rank. A stub is created by a job event before any run event.
const (
	RankStub       int32 = -1
	RankRequested  int32 = 0
	RankInProgress int32 = 1
	RankCompleted  int32 = 2
)

// Job phase. It only rises; a job event for `queued` is ignored by the caller.
const (
	PhaseWaiting    int32 = 1
	PhaseInProgress int32 = 2
	PhaseCompleted  int32 = 3
)

// TxStarter opens the per-workspace transaction; *pgxpool.Pool satisfies it.
type TxStarter interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// RunEvent is a workflow_run delivery reduced to the stored columns.
type RunEvent struct {
	InstallationID int64
	RepoOwner      string
	RepoName       string
	RunID          int64
	RunAttempt     int32
	StatusRank     int32
	Status         string
	Conclusion     string
	GhUpdatedAt    time.Time
	WorkflowPath   string
	WorkflowName   string
	DisplayTitle   string
	HeadSHA        string
	HeadBranch     string
	Event          string
	HTMLURL        string
}

// JobEvent is a workflow_job delivery reduced to the stored columns.
type JobEvent struct {
	InstallationID int64
	RepoOwner      string
	RepoName       string
	RunID          int64
	RunAttempt     int32
	HeadSHA        string
	JobID          int64
	JobName        string
	Phase          int32
}

// ApplyRunEvent writes one workflow_run delivery for one workspace.
func ApplyRunEvent(ctx context.Context, pool TxStarter, workspaceID pgtype.UUID, event RunEvent) error {
	event.RepoOwner, event.RepoName = strings.ToLower(event.RepoOwner), strings.ToLower(event.RepoName)
	return inTx(ctx, pool, func(queries *db.Queries) error {
		run, err := lockRun(ctx, queries, workspaceID, event.InstallationID, event.RepoOwner, event.RepoName, event.RunID, event.RunAttempt, event.HeadSHA)
		if err != nil {
			return err
		}
		key := runKey{WorkspaceID: workspaceID, RepoOwner: event.RepoOwner, RepoName: event.RepoName, RunID: event.RunID}
		switch {
		case event.RunAttempt < run.RunAttempt:
			return nil
		case event.RunAttempt > run.RunAttempt:
			return queries.ReplaceGitHubWorkflowRunAttempt(ctx, db.ReplaceGitHubWorkflowRunAttemptParams{
				WorkspaceID: key.WorkspaceID, RepoOwner: key.RepoOwner, RepoName: key.RepoName, RunID: key.RunID,
				InstallationID: event.InstallationID, RunAttempt: event.RunAttempt,
				StatusRank: event.StatusRank, Status: event.Status, Conclusion: event.Conclusion,
				GhUpdatedAt:  timestamp(event.GhUpdatedAt),
				WorkflowPath: event.WorkflowPath, WorkflowName: event.WorkflowName, DisplayTitle: event.DisplayTitle,
				HeadSha: event.HeadSHA, HeadBranch: event.HeadBranch, Event: event.Event, HtmlUrl: event.HTMLURL,
			})
		}
		return queries.AdvanceGitHubWorkflowRun(ctx, db.AdvanceGitHubWorkflowRunParams{
			WorkspaceID: key.WorkspaceID, RepoOwner: key.RepoOwner, RepoName: key.RepoName, RunID: key.RunID,
			WorkflowPath: event.WorkflowPath, WorkflowName: event.WorkflowName, DisplayTitle: event.DisplayTitle,
			HeadSha: event.HeadSHA, HeadBranch: event.HeadBranch, Event: event.Event, HtmlUrl: event.HTMLURL,
			AdvanceStatus: advancesStatus(run, event),
			StatusRank:    event.StatusRank, Status: event.Status, Conclusion: event.Conclusion,
			GhUpdatedAt: timestamp(event.GhUpdatedAt),
		})
	})
}

// ApplyJobEvent writes one workflow_job delivery for one workspace.
func ApplyJobEvent(ctx context.Context, pool TxStarter, workspaceID pgtype.UUID, event JobEvent) error {
	event.RepoOwner, event.RepoName = strings.ToLower(event.RepoOwner), strings.ToLower(event.RepoName)
	return inTx(ctx, pool, func(queries *db.Queries) error {
		run, err := lockRun(ctx, queries, workspaceID, event.InstallationID, event.RepoOwner, event.RepoName, event.RunID, event.RunAttempt, event.HeadSHA)
		if err != nil {
			return err
		}
		if event.RunAttempt > run.RunAttempt {
			if err := queries.RaiseGitHubWorkflowRunAttemptFromJob(ctx, db.RaiseGitHubWorkflowRunAttemptFromJobParams{
				WorkspaceID: workspaceID, RepoOwner: event.RepoOwner, RepoName: event.RepoName, RunID: event.RunID,
				RunAttempt: event.RunAttempt,
			}); err != nil {
				return err
			}
			run.RunAttempt, run.StatusRank = event.RunAttempt, RankStub
		}
		currentAttempt := event.RunAttempt == run.RunAttempt
		// A gate that opens after its own attempt finished has already passed.
		if event.Phase == PhaseWaiting && currentAttempt && run.StatusRank == RankCompleted {
			return nil
		}
		inserted, err := queries.InsertGitHubWorkflowJob(ctx, db.InsertGitHubWorkflowJobParams{
			WorkspaceID: workspaceID, RepoOwner: event.RepoOwner, RepoName: event.RepoName, RunID: event.RunID,
			RunAttempt: event.RunAttempt, JobID: event.JobID, JobName: event.JobName, Phase: event.Phase,
		})
		if err != nil {
			return err
		}
		if inserted == 0 {
			_, err = queries.AdvanceGitHubWorkflowJobPhase(ctx, db.AdvanceGitHubWorkflowJobPhaseParams{
				WorkspaceID: workspaceID, RepoOwner: event.RepoOwner, RepoName: event.RepoName, JobID: event.JobID,
				Phase: event.Phase, JobName: event.JobName,
			})
			return err
		}
		// A new row can only hold phase 1 as its lowest phase, so inserting a
		// waiting row means a gate opened. Lower-attempt rows are inert.
		if event.Phase == PhaseWaiting && currentAttempt {
			return queries.IncrementGitHubWorkflowRunGateGeneration(ctx, db.IncrementGitHubWorkflowRunGateGenerationParams{
				WorkspaceID: workspaceID, RepoOwner: event.RepoOwner, RepoName: event.RepoName, RunID: event.RunID,
				RunAttempt: event.RunAttempt,
			})
		}
		return nil
	})
}

type runKey struct {
	WorkspaceID pgtype.UUID
	RepoOwner   string
	RepoName    string
	RunID       int64
}

// lockRun inserts a stub when the run is unknown, then locks the row so every
// event for the run serializes behind this transaction.
func lockRun(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, installationID int64, owner, name string, runID int64, attempt int32, headSHA string) (db.GithubWorkflowRun, error) {
	if err := queries.EnsureGitHubWorkflowRunStub(ctx, db.EnsureGitHubWorkflowRunStubParams{
		WorkspaceID: workspaceID, InstallationID: installationID, RepoOwner: owner, RepoName: name,
		RunID: runID, RunAttempt: attempt, HeadSha: headSHA,
	}); err != nil {
		return db.GithubWorkflowRun{}, err
	}
	return queries.LockGitHubWorkflowRun(ctx, db.LockGitHubWorkflowRunParams{
		WorkspaceID: workspaceID, RepoOwner: owner, RepoName: name, RunID: runID,
	})
}

// advancesStatus reports whether a same-attempt run event moves the status
// fields: a higher rank always does, an equal rank only when not older. A
// stored NULL gh_updated_at counts as the oldest. Completed never regresses.
func advancesStatus(stored db.GithubWorkflowRun, event RunEvent) bool {
	switch {
	case event.StatusRank > stored.StatusRank:
		return true
	case event.StatusRank < stored.StatusRank:
		return false
	case !stored.GhUpdatedAt.Valid:
		return true
	}
	return !event.GhUpdatedAt.Before(stored.GhUpdatedAt.Time)
}

func timestamp(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at, Valid: !at.IsZero()}
}

func inTx(ctx context.Context, pool TxStarter, apply func(*db.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := apply(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
