package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// runScriptTask runs a script-mode autopilot task (CHE-4678 S9 P4): one
// allowlisted command under the task token, with no model session. It never
// resolves a provider, skill bundle or execution env. The allowlist lives in
// <profile dir>/scripts; with none present every script task fails closed.
func (d *Daemon) runScriptTask(ctx context.Context, task Task, taskLog *slog.Logger) (TaskResult, error) {
	token, err := taskScopedAuthToken(task)
	if err != nil {
		return TaskResult{}, err
	}
	if _, err := d.startTaskWithCurrentClaim(ctx, task); err != nil {
		return TaskResult{}, fmt.Errorf("start task failed: %w", err)
	}

	workDir, err := os.MkdirTemp("", "multica-script-")
	if err != nil {
		return TaskResult{}, fmt.Errorf("script work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	// An unresolvable profile dir leaves Dir empty, which the backend rejects
	// as script_not_allowlisted.
	var scriptsDir string
	if profileDir, err := cli.ProfileDir(d.cfg.Profile); err == nil {
		scriptsDir = filepath.Join(profileDir, "scripts")
	}
	backend, err := agent.New("script", agent.Config{
		Script: &agent.ScriptSpec{Dir: scriptsDir, Name: task.Script.Name, Args: task.Script.Args, Timeout: d.cfg.ScriptTimeout},
		Env: map[string]string{
			"MULTICA_TOKEN":            token,
			"MULTICA_SERVER_URL":       d.cfg.ServerBaseURL,
			"MULTICA_WORKSPACE_ID":     task.WorkspaceID,
			"MULTICA_TASK_ID":          task.ID,
			"MULTICA_AGENT_ID":         task.AgentID,
			"MULTICA_AUTOPILOT_ID":     task.AutopilotID,
			"MULTICA_AUTOPILOT_RUN_ID": task.AutopilotRunID,
			"TMPDIR":                   workDir,
		},
		Logger: taskLog,
		TaskID: task.ID,
	})
	if err != nil {
		return TaskResult{}, err
	}
	session, err := backend.Execute(ctx, "", agent.ExecOptions{Cwd: workDir})
	if err != nil {
		return TaskResult{}, err
	}
	for range session.Messages {
	}
	result := <-session.Result
	taskLog.Info("script task finished", "task_id", task.ID, "status", result.Status, "duration_ms", result.DurationMs)

	switch result.Status {
	case "completed":
		return TaskResult{Status: "completed", Comment: result.Output}, nil
	case "timeout":
		return TaskResult{Status: "blocked", Comment: result.Error, FailureReason: "timeout"}, nil
	case "cancelled":
		return TaskResult{Status: "cancelled"}, nil
	}
	return TaskResult{Status: "blocked", Comment: result.Error, FailureReason: agent.ScriptFailureReason(result.Error)}, nil
}
