package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type bgFixture struct {
	d       *Daemon
	argsOut string
	lsOut   string
	envOut  string
	secret  string
}

// newBackgroundFixture builds a daemon whose "claude" is a shell script that
// honours the no-tool contract the way the real CLI does: given `--tools ""` it
// has no file tool, otherwise it reads the secret file outside its workdir and
// echoes it. It records its argv, its cwd listing and its MULTICA_* env.
func newBackgroundFixture(t *testing.T) *bgFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script agent fixture is POSIX-only")
	}
	dir := t.TempDir()
	f := &bgFixture{
		argsOut: filepath.Join(dir, "args"),
		lsOut:   filepath.Join(dir, "ls"),
		envOut:  filepath.Join(dir, "env"),
		secret:  filepath.Join(dir, "secret.txt"),
	}
	if err := os.WriteFile(f.secret, []byte("TOP-SECRET-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "claude-fixture")
	script := "#!/bin/sh\n" +
		"case \"$*\" in *--version*) echo '2.1.318'; exit 0;; esac\n" +
		"IFS= read -r _\n" +
		"printf '%s\\n' \"$@\" > '" + f.argsOut + "'\n" +
		"ls -A \"$PWD\" > '" + f.lsOut + "'\n" +
		"env | grep '^MULTICA_' > '" + f.envOut + "'\n" +
		"READ=''\n" +
		"if ! printf '%s\\n' \"$@\" | grep -qx -- '--tools'; then READ=$(cat '" + f.secret + "'); fi\n" +
		`printf '%s\n' '{"type":"system","session_id":"s1"}'` + "\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","result":"title%s"}\n' "$READ"` + "\n"
	writeTestExecutable(t, fake, []byte(script))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.d = &Daemon{
		client: NewClient(srv.URL), logger: logger,
		workspaces: make(map[string]*workspaceState), activeEnvRoots: make(map[string]int),
		runtimeIndex: map[string]Runtime{"rt": {ID: "rt", Provider: "claude"}},
		cfg: Config{
			WorkspacesRoot: t.TempDir(), AgentTimeout: 5 * time.Second, ServerBaseURL: srv.URL,
			Agents: map[string]AgentEntry{
				"claude": {Path: fake},
			},
		},
	}
	return f
}

func backgroundTask() Task {
	return Task{
		ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", WorkspaceID: "ws", RuntimeID: "rt",
		AgentID: "ag", AuthToken: "mat_secret_token",
		BackgroundJob: "dm_title", BackgroundInput: "hello",
		IssueID: "issue-1",
		Repos:   []RepoData{{URL: "https://example.invalid/r.git"}},
		Agent: &AgentData{
			ID: "ag", Name: "bg", Model: "m",
			CustomArgs: []string{"--allowedTools", "Bash", "--tools", "default"},
			McpConfig:  json.RawMessage(`{"mcpServers":{"x":{"command":"evil"}}}`),
			Skills:     []SkillData{{Name: "s", Content: "do things"}},
			CustomEnv:  map[string]string{"MULTICA_TOKEN": "leak"},
		},
	}
}

func TestRunTaskBackgroundIsIsolated(t *testing.T) {
	f := newBackgroundFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	res, err := f.d.runTask(context.Background(), backgroundTask(), "claude", 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" || res.Comment != "title" {
		// A comment carrying the secret means the provider had a file tool.
		t.Fatalf("result = %+v; want completed with comment %q", res, "title")
	}
	if strings.Contains(res.Comment, "TOP-SECRET") {
		t.Fatal("provider read a file outside its workdir")
	}

	argsRaw, _ := os.ReadFile(f.argsOut)
	args := strings.Split(strings.TrimSpace(string(argsRaw)), "\n")
	for _, want := range []string{"--tools", "--strict-mcp-config", "--disable-slash-commands"} {
		if !bgContains(args, want) {
			t.Errorf("launch args lack %s: %q", want, args)
		}
	}
	for _, banned := range []string{"--allowedTools", "--mcp-config", "--add-dir"} {
		if bgContains(args, banned) {
			t.Errorf("launch args must not carry %s: %q", banned, args)
		}
	}
	if ls, _ := os.ReadFile(f.lsOut); strings.TrimSpace(string(ls)) != "" {
		t.Errorf("workdir must be empty, has: %s", ls)
	}
	if env, _ := os.ReadFile(f.envOut); strings.TrimSpace(string(env)) != "" {
		t.Errorf("no MULTICA_* env may reach the provider: %s", env)
	}
}

// A provider that can read files or workspace content has no verified no-tool
// mode, so the daemon refuses to launch it.
func TestRunTaskBackgroundRefusesIneligibleProvider(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, provider := range []string{"codex", "opencode", "cursor", "hermes", "kimi", "copilot", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			f := newBackgroundFixture(t)
			f.d.cfg.Agents[provider] = f.d.cfg.Agents["claude"]
			_, err := f.d.runTask(context.Background(), backgroundTask(), provider, 0, logger)
			if err == nil || !strings.Contains(err.Error(), "output_only") {
				t.Fatalf("err = %v; want an output_only ineligibility error", err)
			}
			if _, statErr := os.Stat(f.argsOut); statErr == nil {
				t.Fatal("ineligible provider must not be launched")
			}
		})
	}
}

func bgContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
