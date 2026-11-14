package execenv

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// CHE-4101: an output_only run gets a new empty workdir: no brief, no context
// files, no skills, no MCP config, no checkout.
func TestPrepareBackgroundWorkdirIsEmpty(t *testing.T) {
	root := t.TempDir()
	env, err := PrepareBackground(PrepareParams{
		WorkspacesRoot: root, WorkspaceID: "ws-1", TaskID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.ReleaseLock)

	entries, err := os.ReadDir(env.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("workdir must be empty, has %v", entries)
	}
	if env.LocalDirectory || env.CodexHome != "" || env.ClaudeSettingsPath != "" {
		t.Fatalf("no provider state may be prepared: %+v", env)
	}
}

func TestPrepareBackgroundRejectsMissingIDs(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, p := range []PrepareParams{
		{WorkspaceID: "ws", TaskID: "t"},
		{WorkspacesRoot: t.TempDir(), TaskID: "t"},
		{WorkspacesRoot: t.TempDir(), WorkspaceID: "ws"},
	} {
		if _, err := PrepareBackground(p, log); err == nil {
			t.Errorf("expected error for %+v", p)
		}
	}
}
