package daemon

import (
	"os"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// armBriefFile is the arm-1 runtime brief inside the task env root; the managed
// profile writes the same file (execenv.managedClaudeBriefFile).
const armBriefFile = "runtime-brief.md"

func costTrialLeverFromEnv() bool { return boolFromEnv("MULTICA_COST_TRIAL_LEVER", false) }

// bootPrefixFor decides the boot prefix of one Claude run (CHE-6540). lever is
// true only when the config turns it on, the run is in trial arm 1, bwrap is
// there and the stable cwd resolves; then the run gets the stable cwd and the
// arm-1 brief layout (see deliverBrief). Every other run is control: the
// stable cwd stays what MULTICA_STABLE_CWD_AGENTS says, and the brief goes
// into the workdir CLAUDE.md.
func (c Config) bootPrefixFor(task Task, provider string, hasBwrap bool) (stableCwd string, lever bool) {
	agentName := ""
	if task.Agent != nil {
		agentName = task.Agent.Name
	}
	if c.CostTrialLever && provider == "claude" && hasBwrap {
		if root, ok := task.RootIssueKey(); ok && TrialArm(root) == 1 {
			if cwd := c.stableCwdFor(provider, agentName, task.AgentID, true); cwd != "" {
				return cwd, true
			}
		}
	}
	return c.stableCwdFor(provider, agentName, task.AgentID, false), false
}

// deliverBrief writes the runtime brief for one run. Control (lever false):
// the whole brief goes to the workdir file for the provider, as before, and
// briefPath and perTask are empty. Lever true: the stable part goes to a file
// in rootDir, for --append-system-prompt-file, no workdir CLAUDE.md is left,
// and perTask is the text for the per-turn prompt (WithTaskBrief).
func deliverBrief(provider string, ctx execenv.TaskContextForEnv, workDir, rootDir string, lever bool) (brief, briefPath, perTask string, err error) {
	if !lever {
		brief, err = execenv.InjectRuntimeConfig(workDir, provider, ctx)
		return brief, "", "", err
	}
	brief, perTask = execenv.SplitRuntimeBrief(provider, ctx)
	// A block left by an earlier control run must not repeat the brief.
	_ = execenv.CleanupRuntimeConfig(workDir, provider)
	briefPath = filepath.Join(rootDir, armBriefFile)
	return brief, briefPath, perTask, os.WriteFile(briefPath, []byte(brief), 0o600)
}

// applyBootLaunch puts the boot-prefix decision into the launch options: the
// stable cwd and the file for --append-system-prompt-file. A lever run uses its
// own brief file; any other run keeps the managed-profile file (empty when the
// profile is not managed).
func applyBootLaunch(opts *agent.ExecOptions, stableCwd, managedBriefPath, leverBriefPath string, lever bool) {
	opts.StableCwd = stableCwd
	opts.AppendSystemPromptFile = managedBriefPath
	if lever {
		opts.AppendSystemPromptFile = leverBriefPath
	}
}

// bootPromptOptions carries the per-task part of a lever brief into the turn prompt.
func bootPromptOptions(perTask string) []PromptOption {
	if perTask == "" {
		return nil
	}
	return []PromptOption{WithTaskBrief(perTask)}
}
