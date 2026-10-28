package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A run that shows no event for this long without a waiting gate probably lost
// its completed delivery; the rule says so instead of waiting silently.
const githubRunStaleAfter = 6 * time.Hour

var (
	githubRepoPart    = regexp.MustCompile(`^[a-z0-9._-]+$`)
	githubWorkflowRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+\.ya?ml$`)
	githubHeadSHARe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	githubWorkflowDir = ".github/workflows/"
)

// validateGitHubRun normalizes a github_run condition. ownerInstalled reports
// whether the workspace has the GitHub App installed for the repo owner.
func validateGitHubRun(c WakeupCondition, ownerInstalled func(owner string) (bool, error)) (WakeupCondition, error) {
	own := WakeupCondition{Type: c.Type, Repo: c.Repo, RunID: c.RunID, Workflow: c.Workflow, HeadSHA: c.HeadSHA, State: c.State}
	if !reflect.DeepEqual(c, own) {
		return own, badCondition("github_run takes only repo, run_id, workflow, head_sha and state")
	}
	repo := strings.ToLower(c.Repo)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !githubRepoPart.MatchString(owner) || !githubRepoPart.MatchString(name) {
		return own, badCondition("repo must be owner/name")
	}
	sha := strings.ToLower(c.HeadSHA)
	switch {
	case c.RunID < 0:
		return own, badCondition("run_id must be a positive number")
	case c.RunID > 0 && (c.Workflow != "" || c.HeadSHA != ""):
		return own, badCondition("select the run by run_id, or by workflow and head_sha, not both")
	case c.RunID == 0 && !githubWorkflowRe.MatchString(c.Workflow):
		return own, badCondition("select the run by run_id, or by workflow (a file name such as ci.yml) and head_sha")
	case c.RunID == 0 && !githubHeadSHARe.MatchString(sha):
		return own, badCondition("head_sha must be the full 40-character commit SHA")
	case c.State != "completed" && c.State != "waiting":
		return own, badCondition("state must be completed or waiting")
	}
	installed, err := ownerInstalled(owner)
	if err != nil {
		return own, err
	}
	if !installed {
		return own, badCondition("the GitHub App is not installed for this repository owner in this workspace")
	}
	own.Repo, own.HeadSHA = repo, sha
	return own, nil
}

// evaluateGitHubRun reads the webhook-fed run mirror, scoped to the rule's
// workspace. Only each run's current attempt counts, and every satisfying
// episode gets its own key (run#attempt:completed, run#attempt:gate:N), so
// conditionNewlyMet fires once per episode, however many polls it spans.
func evaluateGitHubRun(ctx context.Context, tx pgx.Tx, q *db.Queries, w db.IssueWakeup, c WakeupCondition) (bool, string, map[string]any, error) {
	owner, name, _ := strings.Cut(c.Repo, "/")
	var runs []db.GithubWorkflowRun
	if c.RunID != 0 {
		run, err := q.GetGitHubWorkflowRun(ctx, db.GetGitHubWorkflowRunParams{WorkspaceID: w.WorkspaceID, RepoOwner: owner, RepoName: name, RunID: c.RunID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, "", nil, err
		}
		if err == nil {
			runs = append(runs, run)
		}
	} else {
		bySHA, err := q.ListGitHubWorkflowRunsByHeadSHA(ctx, db.ListGitHubWorkflowRunsByHeadSHAParams{WorkspaceID: w.WorkspaceID, RepoOwner: owner, RepoName: name, HeadSha: c.HeadSHA})
		if err != nil {
			return false, "", nil, err
		}
		for _, run := range bySHA {
			if run.WorkflowPath == githubWorkflowDir+c.Workflow {
				runs = append(runs, run)
			}
		}
	}

	var keys []string
	var facts []map[string]any
	stale := false
	for _, run := range runs {
		var key string
		switch {
		case c.State == "completed" && run.StatusRank == 2:
			key = fmt.Sprintf("%d#%d:completed", run.RunID, run.RunAttempt)
		case c.State == "waiting" && run.GateGeneration > 0:
			key = fmt.Sprintf("%d#%d:gate:%d", run.RunID, run.RunAttempt, run.GateGeneration)
		}
		inProgress := run.StatusRank == 1 && time.Since(run.UpdatedAt.Time) > githubRunStaleAfter
		if key == "" && !inProgress {
			continue
		}
		jobs, err := q.ListGitHubWorkflowJobsByRunAttempt(ctx, db.ListGitHubWorkflowJobsByRunAttemptParams{WorkspaceID: w.WorkspaceID, RepoOwner: owner, RepoName: name, RunID: run.RunID, RunAttempt: run.RunAttempt})
		if err != nil {
			return false, "", nil, err
		}
		var waiting []string
		for _, job := range jobs {
			if job.Phase == 1 && run.StatusRank < 2 {
				waiting = append(waiting, job.JobName)
			}
		}
		stale = stale || (inProgress && len(waiting) == 0)
		if key == "" {
			continue
		}
		keys = append(keys, key)
		facts = append(facts, map[string]any{
			"run_id": run.RunID, "run_attempt": run.RunAttempt, "gate_generation": run.GateGeneration,
			"status": run.Status, "conclusion": run.Conclusion, "html_url": run.HtmlUrl,
			"waiting_now": len(waiting) > 0, "waiting_jobs": waiting,
		})
	}
	if len(keys) > 0 {
		slices.Sort(keys)
		var diagnostic error
		if stale {
			diagnostic = errGHRunStale
		}
		return true, "gh:" + strings.Join(keys, ","), map[string]any{"runs": facts}, diagnostic
	}
	switch {
	case len(runs) > 0 && stale:
		return false, "", nil, errGHRunStale
	case len(runs) > 0:
		return false, "", nil, nil
	case w.ConditionState != "":
		return false, "", nil, errGHRunAgedOut
	}
	var seen bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM github_workflow_run WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3)", w.WorkspaceID, owner, name).Scan(&seen)
	if err != nil {
		return false, "", nil, err
	}
	if !seen {
		return false, "", nil, errGHRunNoEvents
	}
	return false, "", nil, errGHRunNoMatch
}

// mergeGitHubRunKeys unions the episode keys a rule has seen with the current
// ones. A key names one run attempt or gate and never repeats, so the set only
// grows: cache eviction of some matching runs, or of all of them, must not make
// a redelivered episode look new.
func mergeGitHubRunKeys(previous, current string) string {
	seen := map[string]bool{}
	for _, fingerprint := range []string{previous, current} {
		for key := range conditionKeys(fingerprint) {
			seen[key] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return "gh:" + strings.Join(keys, ",")
}
