package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// runBackgroundTask runs an output_only background task (CHE-4101) in full
// isolation. It is a separate path, not a flag inside runTask, so nothing
// from the normal pipeline (skills, MCP, repos, runtime brief, task token,
// resume, custom args) can reach the run.
//
// The provider gets: a new empty workdir, the bounded prompt from the task
// context, the agent's model and custom_env, and the provider's no-tool
// launch mode. It gets no MULTICA_* env, no skills, no MCP config and no repo.
func (d *Daemon) runBackgroundTask(ctx context.Context, task Task, provider string, taskLog *slog.Logger) (TaskResult, error) {
	if reason := agent.OutputOnlyIneligibleReason(provider); reason != "" {
		return TaskResult{}, fmt.Errorf("output_only background job %q refused: %s", task.BackgroundJob, reason)
	}
	// A custom runtime profile launches an arbitrary command that only speaks
	// the provider's protocol; its tool flags are unverified.
	if _, custom := d.customProfileLaunchForRuntime(task.RuntimeID); custom {
		return TaskResult{}, fmt.Errorf("output_only background job %q refused: custom runtime profile commands have no verified no-tool mode", task.BackgroundJob)
	}
	entry, ok := d.agents()[provider]
	if !ok {
		return TaskResult{}, fmt.Errorf("no agent configured for provider %q", provider)
	}
	var resolvedVersion string
	var err error
	entry, resolvedVersion, err = d.resolveAgentEntryForLaunch(ctx, provider, entry)
	if err != nil {
		return TaskResult{}, err
	}

	env, err := execenv.PrepareBackground(execenv.PrepareParams{
		WorkspacesRoot: d.cfg.WorkspacesRoot,
		WorkspaceID:    task.WorkspaceID,
		TaskID:         task.ID,
	}, d.logger)
	if err != nil {
		return TaskResult{}, fmt.Errorf("prepare background environment: %w", err)
	}
	defer env.ReleaseLock()

	model := entry.Model
	var customEnv map[string]string
	if task.Agent != nil {
		if task.Agent.Model != "" {
			model = task.Agent.Model
		}
		customEnv = task.Agent.CustomEnv
	}
	agentEnv := map[string]string{}
	layerCustomEnvAndHermesHome(agentEnv, customEnv, "", d.logger)

	backend, err := agent.ResolveBackend(provider, agent.Config{
		ExecutablePath: entry.Path,
		CLIVersion:     resolvedVersion,
		Env:            agentEnv,
		Logger:         d.logger,
		TaskID:         task.ID,
		RuntimeID:      task.RuntimeID,
		DaemonVersion:  d.cfg.CLIVersion,
		BuiltinRuntime: true,
	})
	if err != nil {
		return TaskResult{}, fmt.Errorf("create agent backend: %w", err)
	}

	taskLog.Info("starting output_only background run", "provider", provider, "job", task.BackgroundJob, "model", model)
	var msgSeq atomic.Int32
	result, _, err := d.executeAndDrain(ctx, backend, BuildPrompt(task, provider), agent.ExecOptions{
		OutputOnly: true,
		Cwd:        env.WorkDir,
		Model:      model,
		Timeout:    taskRunTimeout(d.cfg.AgentTimeout, task),
	}, taskLog, task.ID, "", &msgSeq)
	if err != nil {
		return TaskResult{}, err
	}

	out := TaskResult{Status: "completed", Comment: result.Output, WorkDir: env.WorkDir, EnvRoot: env.RootDir}
	for model, u := range result.Usage {
		out.Usage = append(out.Usage, TaskUsageEntry{
			Provider: provider, Model: model,
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
			CostUSDTicks: u.CostUSDTicks,
		})
	}
	if result.Status != "completed" {
		msg := result.Error
		if msg == "" {
			msg = fmt.Sprintf("%s ended with status %q", provider, result.Status)
		}
		out.Status, out.Comment = "blocked", msg
		out.FailureReason = taskfailure.Classify(msg).String()
		if result.Status == "timeout" {
			out.FailureReason = "timeout"
		}
	}
	return out, nil
}
