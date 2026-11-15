package agent

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func claudeDisallowedValues(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--disallowedTools" {
			out = append(out, args[i+1])
		}
	}
	return out
}

// Elevated Claude runs carry WebSearch and WebFetch in --disallowedTools, last,
// so no user argument can come after it.
func TestBuildClaudeArgsElevatedDisablesProviderWebTools(t *testing.T) {
	t.Parallel()

	args := buildClaudeArgs(ExecOptions{
		DisableProviderWebTools: true,
		CustomArgs:              []string{"--allowedTools", "WebSearch,WebFetch", "--model", "x"},
	}, slog.Default())

	if !slices.Contains(claudeDisallowedValues(args), "WebSearch,WebFetch") {
		t.Fatalf("missing --disallowedTools WebSearch,WebFetch: %v", args)
	}
	n := len(args)
	if n < 2 || args[n-2] != "--disallowedTools" || args[n-1] != "WebSearch,WebFetch" {
		t.Fatalf("web-tool denial must be the last flag pair: %v", args)
	}
	if !slices.Contains(claudeDisallowedValues(args), "AskUserQuestion") {
		t.Fatalf("existing AskUserQuestion denial lost: %v", args)
	}
}

func TestBuildClaudeArgsDefaultKeepsProviderWebTools(t *testing.T) {
	t.Parallel()

	args := buildClaudeArgs(ExecOptions{}, slog.Default())
	if strings.Contains(strings.Join(args, " "), "WebSearch") {
		t.Fatalf("restricted run must not change web tools: %v", args)
	}
}

func codexWebSearchConfigs(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if (args[i] == "-c" || args[i] == "--config") && strings.Contains(args[i+1], "web_search") {
			out = append(out, args[i+1])
		}
	}
	return out
}

// Elevated Codex runs end with web_search="disabled"; user overrides of the
// key and the --search flag are dropped, unrelated overrides pass through.
func TestBuildCodexArgsElevatedDisablesWebSearch(t *testing.T) {
	t.Parallel()

	args := buildCodexArgs(ExecOptions{
		DisableProviderWebTools: true,
		ExtraArgs:               []string{"--search", "-c", `web_search="live"`},
		CustomArgs:              []string{`--config=web_search="cached"`, "-c", `tools.web_search=true`, "-c", `model="o3"`},
	}, slog.Default())

	if got := codexWebSearchConfigs(args); !slices.Equal(got, []string{`web_search="disabled"`}) {
		t.Fatalf("web_search configs = %v, args %v", got, args)
	}
	n := len(args)
	if args[n-2] != "-c" || args[n-1] != `web_search="disabled"` {
		t.Fatalf("disable must be last: %v", args)
	}
	for _, a := range args {
		if a == "--search" || strings.HasPrefix(a, "--config=web_search") {
			t.Fatalf("user web search override survived: %v", args)
		}
	}
	if !slices.Contains(args, `model="o3"`) {
		t.Fatalf("unrelated override dropped: %v", args)
	}
}

func TestBuildCodexArgsDefaultLeavesWebSearchAlone(t *testing.T) {
	t.Parallel()

	args := buildCodexArgs(ExecOptions{CustomArgs: []string{"--search"}}, slog.Default())
	if !slices.Contains(args, "--search") || len(codexWebSearchConfigs(args)) != 0 {
		t.Fatalf("restricted run must not change web search: %v", args)
	}
}

// A runtime profile's launch prefix is filtered like the other managed keys.
func TestStripCodexWebSearchOverridesCoversPrefix(t *testing.T) {
	t.Parallel()

	got := stripCodexWebSearchOverrides([]string{"--search", "-c", `web_search="live"`, "-c", "a=1"}, slog.Default())
	if !slices.Equal(got, []string{"-c", "a=1"}) {
		t.Fatalf("got %v", got)
	}
}
