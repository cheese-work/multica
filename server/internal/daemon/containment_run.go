package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon/containment"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// parseContainmentMode reads MULTICA_CONTAINMENT: off (default) or on. The run
// has one launch hook, which sandbox enforcement on already owns, so the two do
// not combine.
func parseContainmentMode(v string, sbx sandbox.Mode) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "0", "false":
		return false, nil
	case "on", "1", "true":
		if sbx == sandbox.ModeOn {
			return false, fmt.Errorf("containment cannot be combined with MULTICA_SANDBOX_MODE=on (one launch hook per run)")
		}
		return true, nil
	default:
		return false, fmt.Errorf("unknown value %q (want off or on)", v)
	}
}

// containmentRun is one attempt's cgroup leaf and the glue the daemon needs
// around it (CHE-7168 plan r13 3.2). A nil *containmentRun is the flag-off path:
// every method is a no-op that changes nothing.
type containmentRun struct {
	leaf *containment.Leaf
}

// beginContainment creates the attempt's leaf and its durable record before any
// process of the attempt exists. The returned context makes the preparation
// helper fork into the leaf. Flag off: (nil, ctx, nil) and nothing touched.
func (d *Daemon) beginContainment(ctx context.Context, task Task) (*containmentRun, context.Context, error) {
	if !d.cfg.Containment {
		return nil, ctx, nil
	}
	root := d.cfg.ContainmentRoot
	if root == "" {
		var err error
		if root, err = containment.DefaultRoot(); err != nil {
			return nil, ctx, fmt.Errorf("containment: %w", err)
		}
	}
	stateDir := d.cfg.ContainmentStateDir
	if stateDir == "" {
		dir, err := cli.ProfileDir(d.cfg.Profile)
		if err != nil {
			return nil, ctx, fmt.Errorf("containment: %w", err)
		}
		stateDir = filepath.Join(dir, "containment", "attempts")
	}
	leaf, err := containment.Create(containment.Options{
		Root:           root,
		StateDir:       stateDir,
		DaemonInstance: fmt.Sprintf("%s:%d:%d", d.cfg.DaemonID, os.Getpid(), d.startedAt.Unix()),
		TaskID:         task.ID,
	})
	if err != nil {
		return nil, ctx, fmt.Errorf("containment: %w", err)
	}
	return &containmentRun{leaf: leaf}, execenv.WithLaunchAttach(ctx, leaf.Attach), nil
}

// Hook is the agent CLI's launch hook: it forks the CLI into the leaf. Nil when
// the flag is off, so backendConfig.Launch stays nil as today.
func (r *containmentRun) Hook() agent.LaunchHook {
	if r == nil {
		return nil
	}
	return r.leaf
}

// frontShims puts the blocked-launch shims (docker, tmux, ssh and the rest of
// containment.ShimNames) first on the run's PATH.
func (r *containmentRun) frontShims(agentEnv map[string]string, taskTempDir string) error {
	if r == nil {
		return nil
	}
	dir := filepath.Join(taskTempDir, "containment-bin")
	if err := containment.InstallShims(dir, r.leaf.EventsFile()); err != nil {
		return err
	}
	path, ok := agentEnv["PATH"]
	if !ok {
		path = os.Getenv("PATH")
	}
	agentEnv["PATH"] = dir + string(os.PathListSeparator) + path
	return nil
}

// End releases the leaf. Safe on nil and on repeat.
func (r *containmentRun) End(log *slog.Logger) {
	if r == nil {
		return
	}
	if err := r.leaf.End(); err != nil {
		log.Warn("containment: end leaf", "attempt", r.leaf.Record().Attempt, "error", err)
	}
}
