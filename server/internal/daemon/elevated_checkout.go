package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/daemon/broker"
	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

// serveElevatedCheckout answers a repo checkout of an elevated task with the
// synthetic base of its grant's pinned commit (CHE-2216 plan 3.5, B7a): one
// root commit, no upstream history, no alternates. The caller has already
// checked that the task carries a pin and that the repository is ready.
func (d *Daemon) serveElevatedCheckout(w http.ResponseWriter, ctx context.Context, task activeRepoCheckoutTask, repoURL, workDir string) {
	barePath := d.repoCache.BarePath(task.WorkspaceID, repoURL)
	if barePath == "" {
		http.Error(w, "repo not found in cache", http.StatusInternalServerError)
		return
	}
	dest := filepath.Join(workDir, repocache.RepoDirName(repoURL))
	branch := repocache.TaskBranchName(task.AgentName, task.IssueIdentifier, task.TaskID)
	sha := broker.PinnedSource(task.PinnedBaseSHA, task.PinnedHeadSHA)
	err := d.repoCache.WithRepoLock(barePath, func() error {
		_, err := broker.MaterializeSyntheticBase(ctx, barePath, sha, dest, branch)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		d.logger.Error("elevated repo checkout failed", "task_id", task.TaskID, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&repocache.WorktreeResult{Path: dest, BranchName: branch})
}

// newActiveRepoCheckoutTask binds a claimed task's checkout identity and pins.
func newActiveRepoCheckoutTask(task Task, workDir string) activeRepoCheckoutTask {
	return activeRepoCheckoutTask{
		WorkspaceID:     task.WorkspaceID,
		TaskID:          task.ID,
		AgentID:         task.AgentID,
		AgentName:       task.Agent.Name,
		WorkDir:         workDir,
		IssueIdentifier: task.IssueIdentifier,
		IntakeRun:       task.IntakeRun,
		PinnedBranch:    task.PinnedBranch,
		PinnedRepo:      task.PinnedRepo,
		Elevated:        task.EffectiveProfile() == "elevated",
		PinnedBaseSHA:   task.PinnedBaseSHA,
		PinnedHeadSHA:   task.PinnedHeadSHA,
	}
}
