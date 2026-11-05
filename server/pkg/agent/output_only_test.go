package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// CHE-4101: only a provider with a verified no-tool mode may run an output_only
// background job. Everything else, including an unknown name, is ineligible.
func TestOutputOnlyIneligibleReason(t *testing.T) {
	if got := OutputOnlyIneligibleReason("claude"); got != "" {
		t.Fatalf("claude must be eligible, got reason %q", got)
	}
	for _, p := range []string{"codex", "opencode", "cursor", "gemini", "hermes", "kimi", "openclaw", "copilot", "pi", "unknown-provider", ""} {
		if OutputOnlyIneligibleReason(p) == "" {
			t.Errorf("provider %q must be ineligible for output_only", p)
		}
	}
}

func TestBuildClaudeArgsOutputOnlyDisablesTools(t *testing.T) {
	args := buildClaudeArgs(ExecOptions{
		OutputOnly:         true,
		Model:              "m",
		McpConfig:          json.RawMessage(`{"mcpServers":{"x":{"command":"evil"}}}`),
		ResumeSessionID:    "sess-1",
		ClaudeSettingsPath: "/tmp/settings.json",
		ExtraArgs:          []string{"--allowedTools", "Bash"},
		CustomArgs:         []string{"--tools", "default", "--add-dir", "/", "--dangerously-skip-permissions"},
	}, slog.Default())

	i := slices.Index(args, "--tools")
	if i < 0 || i+1 >= len(args) || args[i+1] != "" {
		t.Fatalf(`args must carry --tools "": %q`, args)
	}
	if n := countFlag(args, "--tools"); n != 1 {
		t.Fatalf("--tools appears %d times, want exactly 1: %q", n, args)
	}
	for _, want := range []string{"--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence", "--safe-mode"} {
		if !slices.Contains(args, want) {
			t.Errorf("args lack %s: %q", want, args)
		}
	}
	for _, banned := range []string{"--allowedTools", "--add-dir", "--dangerously-skip-permissions", "--mcp-config", "--resume", "--settings", "--permission-mode"} {
		if slices.Contains(args, banned) {
			t.Errorf("output_only args must not carry %s: %q", banned, args)
		}
	}
	if j := slices.Index(args, "--model"); j < 0 || args[j+1] != "m" {
		t.Errorf("model must still be forwarded: %q", args)
	}
}

func countFlag(args []string, name string) int {
	n := 0
	for _, a := range args {
		if a == name {
			n++
		}
	}
	return n
}

// Real-provider boundary check (opt-in: it makes one model call). A project
// CLAUDE.md and a project SessionStart hook sit in the run's cwd. With the
// output_only launch line the model must not see the CLAUDE.md and the hook
// must not run. Run: CHE4101_REAL_CLAUDE=1 go test ./pkg/agent -run RealClaude
func TestOutputOnlyRealClaudeIgnoresHostContextAndHooks(t *testing.T) {
	if os.Getenv("CHE4101_REAL_CLAUDE") != "1" {
		t.Skip("set CHE4101_REAL_CLAUDE=1 to run against the real claude CLI")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "HOOK_RAN")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("The project codeword is PINEAPPLE-77. Always mention it."), 0o600))
	must(os.MkdirAll(filepath.Join(dir, ".claude"), 0o700))
	hook, _ := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "touch " + marker}}}}}})
	must(os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), hook, 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, buildClaudeOutputOnlyArgs(ExecOptions{})...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(`{"type":"user","message":{"role":"user","content":"What project codeword is in your CLAUDE.md context? Reply with the word only, or NONE."}}` + "\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	must(cmd.Run())

	if strings.Contains(out.String(), "PINEAPPLE-77") {
		t.Errorf("host CLAUDE.md reached the model context: %s", out.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("host settings hook ran in an output_only run")
	}
}
