package execenv

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// agy has no per-run MCP flag; it reads plugin MCP servers from
// <cwd>/.agents/plugins/<name>/mcp_config.json (CHE-1821). These tests pin the
// isolation contract the plugin location depends on: the file lives only in a
// workdir private to one task, so agy discovery (which also walks UP to the
// repo root) can never hand it to another run.

const (
	agyPluginRel   = ".agents/plugins/multica-mcp"
	agyMcpSentinel = "sentinel-secret-7f3a"
)

var agyMcpConfig = json.RawMessage(`{"mcpServers":{"hindsight-org":{"url":"https://example.test/mcp","headers":{"Authorization":"Bearer ` + agyMcpSentinel + `"}}}}`)

func agyPrepare(t *testing.T, params PrepareParams, logger *slog.Logger) (*Environment, error) {
	t.Helper()
	params.WorkspacesRoot = t.TempDir()
	params.WorkspaceID = "ws-agy-mcp"
	params.TaskID = "aaaa1111-2222-3333-4444-555566667777"
	params.AgentName = "Agy"
	params.Provider = "antigravity"
	params.Task = TaskContextForEnv{IssueID: "agy-mcp-issue", AgentName: "Agy"}
	return Prepare(params, logger)
}

func TestPrepareAntigravityManagedMcpWritesPrivatePlugin(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	env, err := agyPrepare(t, PrepareParams{McpConfig: agyMcpConfig},
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	dir := filepath.Join(env.WorkDir, filepath.FromSlash(agyPluginRel))

	got, err := os.ReadFile(filepath.Join(dir, "mcp_config.json"))
	if err != nil {
		t.Fatalf("managed mcp_config was not written for agy: %v", err)
	}
	if string(got) != string(agyMcpConfig) {
		t.Fatalf("mcp_config = %s, want the agent's config verbatim", got)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{
			filepath.Join(dir, "mcp_config.json"): 0o600,
			filepath.Join(dir, "plugin.json"):     0o600,
			dir:                                   0o700,
		} {
			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatalf("stat %s: %v", path, statErr)
			}
			if info.Mode().Perm() != want {
				t.Errorf("%s mode = %o, want %o (the file can hold header and env secrets)", path, info.Mode().Perm(), want)
			}
		}
	}
	manifest, err := os.ReadFile(filepath.Join(env.RootDir, sidecarManifestFile))
	if err != nil {
		t.Fatalf("read sidecar manifest: %v", err)
	}
	if !strings.Contains(string(manifest), "multica-mcp") {
		t.Errorf("sidecar manifest does not record the plugin, so cleanup cannot remove it: %s", manifest)
	}
	if strings.Contains(logs.String(), agyMcpSentinel) {
		t.Errorf("Prepare logged an MCP header secret:\n%s", logs.String())
	}
}

func TestPrepareAntigravityWithoutManagedMcpWritesNoPlugin(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]json.RawMessage{"absent": nil, "json null": json.RawMessage(`null`)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, err := agyPrepare(t, PrepareParams{McpConfig: raw}, discardLogger())
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(env.WorkDir, ".agents")); !os.IsNotExist(statErr) {
				t.Fatalf("an unmanaged agy run got a .agents tree (stat err: %v)", statErr)
			}
		})
	}
}

// The managed set is only for agy: another provider must not grow a plugin dir.
func TestPrepareManagedMcpOnOtherProviderWritesNoAntigravityPlugin(t *testing.T) {
	t.Parallel()
	env, err := Prepare(PrepareParams{
		WorkspacesRoot: t.TempDir(),
		WorkspaceID:    "ws-agy-mcp-other",
		TaskID:         "bbbb1111-2222-3333-4444-555566667777",
		AgentName:      "Other",
		Provider:       "claude",
		McpConfig:      agyMcpConfig,
		Task:           TaskContextForEnv{IssueID: "agy-mcp-other"},
	}, discardLogger())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(env.WorkDir, filepath.FromSlash(agyPluginRel))); !os.IsNotExist(statErr) {
		t.Fatalf("a non-agy provider got the agy plugin (stat err: %v)", statErr)
	}
}

// An in-place local_directory is the user's own tree: agy walks up from any
// descendant, so a concurrent human or unmanaged run in that repo would load
// the managed servers and their secrets. No isolation is possible there, so the
// run must fail closed and leave the directory untouched.
func TestPrepareAntigravityManagedMcpFailsClosedInPlace(t *testing.T) {
	t.Parallel()
	userDir := t.TempDir()
	writeFile(t, filepath.Join(userDir, "README.md"), "user content")

	_, err := agyPrepare(t, PrepareParams{LocalWorkDir: userDir, McpConfig: agyMcpConfig}, discardLogger())
	if err == nil {
		t.Fatal("Prepare succeeded; a managed agy mcp_config in a shared local directory cannot be isolated")
	}
	if strings.Contains(err.Error(), agyMcpSentinel) {
		t.Errorf("error leaks an MCP secret: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(userDir, ".agents")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused run left .agents in the user's directory (stat err: %v)", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(userDir, TaskContextMarkerRelPath)); !os.IsNotExist(statErr) {
		t.Fatalf("a refused run left the task marker in the user's directory (stat err: %v)", statErr)
	}
	if data, readErr := os.ReadFile(filepath.Join(userDir, "README.md")); readErr != nil || string(data) != "user content" {
		t.Fatalf("user content changed: data=%q err=%v", data, readErr)
	}
	// An unmanaged agy run in place is unaffected.
	if _, err := agyPrepare(t, PrepareParams{LocalWorkDir: userDir}, discardLogger()); err != nil {
		t.Fatalf("unmanaged in-place Prepare: %v", err)
	}
}

func agyWorktreePrepare(t *testing.T, repo string, config json.RawMessage) (*Environment, error) {
	t.Helper()
	return agyPrepare(t, PrepareParams{
		LocalWorktree: &LocalWorktreeParams{LocalPath: repo},
		McpConfig:     config,
	}, worktreeTestLogger())
}

// A worktree is private to one task, so the plugin is allowed there. Cleanup
// must remove exactly what was written and leave the repo's own tracked
// customization files alone.
func TestAntigravityManagedMcpWorktreeCleanupKeepsExistingCustomizations(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	writeFile(t, filepath.Join(repo, ".agents/plugins/team-kit/plugin.json"), `{"name":"team-kit"}`)
	writeFile(t, filepath.Join(repo, ".agents/plugins/team-kit/mcp_config.json"), `{"mcpServers":{"team":{"command":"true"}}}`)
	gitRun(t, repo, "add", ".agents")
	gitRun(t, repo, "commit", "-m", "team plugin")

	env, err := agyWorktreePrepare(t, repo, agyMcpConfig)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ours := filepath.Join(env.WorkDir, filepath.FromSlash(agyPluginRel), "mcp_config.json")
	if _, statErr := os.Stat(ours); statErr != nil {
		t.Fatalf("plugin missing in worktree: %v", statErr)
	}
	// What the daemon runs on every exit path before Finalize: success,
	// failure and cancellation all unwind through this defer.
	if err := CleanupSidecars(env.RootDir); err != nil {
		t.Fatalf("CleanupSidecars: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(env.WorkDir, filepath.FromSlash(agyPluginRel))); !os.IsNotExist(statErr) {
		t.Fatalf("managed plugin survived cleanup (stat err: %v)", statErr)
	}
	for _, kept := range []string{".agents/plugins/team-kit/plugin.json", ".agents/plugins/team-kit/mcp_config.json"} {
		if _, statErr := os.Stat(filepath.Join(env.WorkDir, filepath.FromSlash(kept))); statErr != nil {
			t.Errorf("cleanup deleted the repo's own %s: %v", kept, statErr)
		}
	}
	if status := gitRun(t, env.WorkDir, "status", "--porcelain"); strings.Contains(status, ".agents") {
		t.Errorf("worktree still dirty under .agents after cleanup:\n%s", status)
	}
}

func TestAntigravityManagedMcpWorktreeRefusesExistingPluginName(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	existing := `{"mcpServers":{"mine":{"command":"true"}}}`
	writeFile(t, filepath.Join(repo, filepath.FromSlash(agyPluginRel), "mcp_config.json"), existing)
	gitRun(t, repo, "add", ".agents")
	gitRun(t, repo, "commit", "-m", "colliding plugin")

	if _, err := agyWorktreePrepare(t, repo, agyMcpConfig); err == nil {
		t.Fatal("Prepare succeeded; it would have overwritten the repo's own plugin")
	}
	if data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(agyPluginRel), "mcp_config.json")); err != nil || string(data) != existing {
		t.Fatalf("the repo's own plugin changed: data=%q err=%v", data, err)
	}
}

// Reuse re-dispatches on the same private workdir. The new config must replace
// the old one, a config removed since must take the plugin away, and files the
// agent itself added next to ours must survive.
func TestReuseAntigravityRefreshesAndDropsManagedMcp(t *testing.T) {
	t.Parallel()
	env, err := agyPrepare(t, PrepareParams{McpConfig: agyMcpConfig}, discardLogger())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	mine := filepath.Join(env.WorkDir, ".agents/plugins/mine/plugin.json")
	writeFile(t, mine, `{"name":"mine"}`)
	ours := filepath.Join(env.WorkDir, filepath.FromSlash(agyPluginRel), "mcp_config.json")
	reuse := func(config json.RawMessage) {
		t.Helper()
		if Reuse(ReuseParams{
			WorkDir:   env.WorkDir,
			Provider:  "antigravity",
			McpConfig: config,
			Task:      TaskContextForEnv{IssueID: "agy-mcp-issue", AgentName: "Agy"},
		}, discardLogger()) == nil {
			t.Fatal("Reuse returned nil")
		}
	}

	refreshed := json.RawMessage(`{"mcpServers":{"other":{"command":"true"}}}`)
	reuse(refreshed)
	if got, err := os.ReadFile(ours); err != nil || string(got) != string(refreshed) {
		t.Fatalf("after Reuse mcp_config = %q (err %v), want the refreshed config", got, err)
	}
	reuse(nil)
	if _, statErr := os.Stat(ours); !os.IsNotExist(statErr) {
		t.Fatalf("a config removed from the agent left a stale plugin behind (stat err: %v)", statErr)
	}
	if _, statErr := os.Stat(mine); statErr != nil {
		t.Fatalf("Reuse deleted the agent's own plugin: %v", statErr)
	}
}
