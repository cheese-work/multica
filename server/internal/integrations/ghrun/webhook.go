package ghrun

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Retention is how long a mirrored run or job row lives after its last event.
const Retention = 30 * 24 * time.Hour

type deliveryRepository struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type runDelivery struct {
	Action      string `json:"action"`
	WorkflowRun struct {
		ID           int64  `json:"id"`
		RunAttempt   int32  `json:"run_attempt"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		UpdatedAt    string `json:"updated_at"`
		Path         string `json:"path"`
		Name         string `json:"name"`
		DisplayTitle string `json:"display_title"`
		HeadSHA      string `json:"head_sha"`
		HeadBranch   string `json:"head_branch"`
		Event        string `json:"event"`
		HTMLURL      string `json:"html_url"`
	} `json:"workflow_run"`
	Repository   deliveryRepository `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type jobDelivery struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID         int64  `json:"id"`
		RunID      int64  `json:"run_id"`
		RunAttempt int32  `json:"run_attempt"`
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
	} `json:"workflow_job"`
	Repository   deliveryRepository `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// ParseRunEvent reduces a workflow_run delivery body to a RunEvent. It reports
// false for a body redelivery cannot fix: malformed JSON, or no installation,
// repository or run id.
func ParseRunEvent(body []byte) (RunEvent, bool) {
	var delivery runDelivery
	if json.Unmarshal(body, &delivery) != nil {
		return RunEvent{}, false
	}
	run, repo := delivery.WorkflowRun, delivery.Repository
	if delivery.Installation.ID == 0 || run.ID == 0 || repo.Name == "" || repo.Owner.Login == "" {
		return RunEvent{}, false
	}
	status := run.Status
	if status == "" {
		status = delivery.Action
	}
	updatedAt, _ := time.Parse(time.RFC3339, run.UpdatedAt)
	return RunEvent{
		InstallationID: delivery.Installation.ID,
		RepoOwner:      repo.Owner.Login, RepoName: repo.Name,
		RunID: run.ID, RunAttempt: attemptOrOne(run.RunAttempt),
		StatusRank: runRank(status), Status: status, Conclusion: run.Conclusion,
		GhUpdatedAt:  updatedAt,
		WorkflowPath: run.Path, WorkflowName: run.Name, DisplayTitle: run.DisplayTitle,
		HeadSHA: run.HeadSHA, HeadBranch: run.HeadBranch, Event: run.Event, HTMLURL: run.HTMLURL,
	}, true
}

// ParseJobEvent reduces a workflow_job delivery body to a JobEvent. A queued
// job is not a phase the mirror stores, so it reports false like a bad body.
func ParseJobEvent(body []byte) (JobEvent, bool) {
	var delivery jobDelivery
	if json.Unmarshal(body, &delivery) != nil {
		return JobEvent{}, false
	}
	job, repo := delivery.WorkflowJob, delivery.Repository
	if delivery.Installation.ID == 0 || job.ID == 0 || job.RunID == 0 || repo.Name == "" || repo.Owner.Login == "" {
		return JobEvent{}, false
	}
	phase := jobPhase(delivery.Action)
	if phase == 0 {
		return JobEvent{}, false
	}
	return JobEvent{
		InstallationID: delivery.Installation.ID,
		RepoOwner:      repo.Owner.Login, RepoName: repo.Name,
		RunID: job.RunID, RunAttempt: attemptOrOne(job.RunAttempt), HeadSHA: job.HeadSHA,
		JobID: job.ID, JobName: job.Name, Phase: phase,
	}, true
}

// DeleteExpired removes run and job rows last touched before cutoff and
// returns how many rows it removed.
func DeleteExpired(ctx context.Context, queries *db.Queries, cutoff time.Time) (int64, error) {
	at := timestamp(cutoff)
	runs, err := queries.DeleteExpiredGitHubWorkflowRuns(ctx, at)
	if err != nil {
		return 0, err
	}
	jobs, err := queries.DeleteExpiredGitHubWorkflowJobs(ctx, at)
	return runs + jobs, err
}

func attemptOrOne(attempt int32) int32 {
	if attempt < 1 {
		return 1
	}
	return attempt
}

func runRank(status string) int32 {
	switch strings.ToLower(status) {
	case "completed":
		return RankCompleted
	case "in_progress":
		return RankInProgress
	}
	return RankRequested
}

func jobPhase(action string) int32 {
	switch action {
	case "waiting":
		return PhaseWaiting
	case "in_progress":
		return PhaseInProgress
	case "completed":
		return PhaseCompleted
	}
	return 0
}
