package execenv

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"testing"
	"time"
)

// CHE-7234: prep processes run in the attempt's cgroup leaf. The attach hook on
// the context runs on the helper's *exec.Cmd before it starts, so the helper and
// every git process under it are born inside the leaf.
func TestPrepareIsolatedAppliesLaunchAttachBeforeStart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var calls int
	var startedAtCall bool
	ctx := WithLaunchAttach(context.Background(), func(cmd *exec.Cmd) error {
		calls++
		startedAtCall = cmd.Process != nil
		return nil
	})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := PrepareIsolated(ctx, preparationHelperTestCommand(), PrepareParams{
		WorkspacesRoot: t.TempDir(),
		WorkspaceID:    "ws-attach",
		TaskID:         "99999999-8888-7777-6666-555555555500",
		Provider:       "claude",
		Task:           TaskContextForEnv{IssueID: "issue-attach"},
	}, logger)
	if err != nil {
		t.Fatalf("PrepareIsolated: %v", err)
	}
	if calls != 1 || startedAtCall {
		t.Fatalf("attach calls = %d, process already started = %v; want one call before Start", calls, startedAtCall)
	}
}

func TestReuseIsolatedAppliesLaunchAttachBeforeStart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var calls int
	ctx := WithLaunchAttach(context.Background(), func(cmd *exec.Cmd) error {
		calls++
		if cmd.Process != nil {
			t.Error("process already started")
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _ = ReuseIsolated(ctx, preparationHelperTestCommand(), ReuseParams{
		WorkspacesRoot: t.TempDir(),
		WorkDir:        t.TempDir(),
		Provider:       "claude",
	}, logger)
	if calls != 1 {
		t.Fatalf("attach calls = %d, want 1", calls)
	}
}

// A leaf that cannot take the helper stops the preparation: nothing runs
// outside the leaf.
func TestLaunchAttachFailureStopsPreparation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	boom := errors.New("leaf gone")
	ctx := WithLaunchAttach(context.Background(), func(*exec.Cmd) error { return boom })
	workspaces := t.TempDir()
	_, err := PrepareIsolated(ctx, preparationHelperTestCommand(), PrepareParams{
		WorkspacesRoot: workspaces,
		WorkspaceID:    "ws-attach-fail",
		TaskID:         "99999999-8888-7777-6666-555555555501",
		Provider:       "claude",
		Task:           TaskContextForEnv{IssueID: "issue-attach-fail"},
	}, logger)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}

// No hook on the context: the command is untouched (flag-off parity).
func TestNoLaunchAttachLeavesCommandUntouched(t *testing.T) {
	if LaunchAttachFrom(context.Background()) != nil {
		t.Fatal("a bare context carries an attach hook")
	}
}
