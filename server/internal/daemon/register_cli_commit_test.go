package daemon

import (
	"context"
	"runtime"
	"testing"
)

// The server compares each runtime's build commit with its own; it can only do
// that when the daemon says which commit, OS and arch it runs.
func TestRegisterAdvertisesBuildIdentity(t *testing.T) {
	fx := newBatchFixture(t)
	d := fx.daemon
	d.cfg.CLICommit = "0123456789abcdef0123456789abcdef01234567"
	d.cfg.Agents = map[string]AgentEntry{"claude": {Path: "/fake/claude"}}
	fx.setWorkspaces(WorkspaceInfo{ID: "ws-1", Name: "one"})

	if err := d.syncWorkspacesFromAPI(context.Background(), false); err != nil {
		t.Fatalf("syncWorkspacesFromAPI: %v", err)
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.registered) == 0 {
		t.Fatal("no Register call reached the server")
	}
	got := fx.registered[0]
	if got.cliCommit != d.cfg.CLICommit || got.cliOS != runtime.GOOS || got.cliArch != runtime.GOARCH {
		t.Fatalf("register advertised commit=%q os=%q arch=%q, want %q %q %q",
			got.cliCommit, got.cliOS, got.cliArch, d.cfg.CLICommit, runtime.GOOS, runtime.GOARCH)
	}
}
