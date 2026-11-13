package ghrun

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Retention is how long a mirrored run or job row lives after its last event.
const Retention = 30 * 24 * time.Hour

type deliveryRepository struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
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
		// The fields below feed ParseReleaseReceipt only.
		WorkflowName string `json:"workflow_name"`
		HeadBranch   string `json:"head_branch"`
		Conclusion   string `json:"conclusion"`
		Steps        []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"steps"`
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
		RepoOwner:      repo.Owner.Login, RepoName: repo.Name, DefaultBranch: repo.DefaultBranch,
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

// ReleaseReceipt is a verified C00 deploy read from a completed deploy job.
type ReleaseReceipt struct {
	InstallationID      int64
	RepoOwner, RepoName string
	RunID               int64
	OldSHA, NewSHA      string
	Digest              string
}

// The cd-deploy `deploy` job names a step with the deployed tuple, but only
// when the deploy ran and the readback passed. The workflow_run payload carries
// neither the digest nor the skip decision, so this step name is the receipt.
const (
	deployWorkflowName = "CD deploy to C00"
	deployJobName      = "deploy"
	deployBranch       = "main"
)

var receiptStep = regexp.MustCompile(`^release-receipt old=([0-9a-f]{40}) new=([0-9a-f]{40}) digest=(sha256:[0-9a-f]{64})$`)

// ParseReleaseReceipt reads a completed workflow_job delivery. It reports true
// only for a successful main-branch `deploy` job of the C00 deploy workflow
// whose receipt step succeeded and names a well-formed old SHA, new SHA and
// digest. A skipped deploy never runs the receipt step. The job head_sha is not
// the binding: the receipt is.
func ParseReleaseReceipt(body []byte) (ReleaseReceipt, bool) {
	var d jobDelivery
	if json.Unmarshal(body, &d) != nil || d.Action != "completed" {
		return ReleaseReceipt{}, false
	}
	job, repo := d.WorkflowJob, d.Repository
	if d.Installation.ID == 0 || job.RunID == 0 || repo.Name == "" || repo.Owner.Login == "" ||
		job.WorkflowName != deployWorkflowName || job.Name != deployJobName ||
		job.HeadBranch != deployBranch || job.Conclusion != "success" {
		return ReleaseReceipt{}, false
	}
	for _, step := range job.Steps {
		if m := receiptStep.FindStringSubmatch(step.Name); m != nil && step.Conclusion == "success" {
			return ReleaseReceipt{
				InstallationID: d.Installation.ID,
				RepoOwner:      strings.ToLower(repo.Owner.Login), RepoName: strings.ToLower(repo.Name),
				RunID: job.RunID, OldSHA: m[1], NewSHA: m[2], Digest: m[3],
			}, true
		}
	}
	return ReleaseReceipt{}, false
}
