package agent

import (
	"encoding/json"
	"log/slog"
	"slices"
	"testing"
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
	for _, want := range []string{"--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"} {
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
