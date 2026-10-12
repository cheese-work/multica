package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCodexLastRequestIsNotTurnTotal(t *testing.T) {
	client := &codexClient{}
	client.setActiveTurnID("current")
	for _, input := range []float64{100, 150} {
		params := codexThreadTokenUsageParams("thread", "current", map[string]any{"inputTokens": input * 100}, map[string]any{"inputTokens": input, "cachedInputTokens": float64(50)})
		params["tokenUsage"].(map[string]any)["modelContextWindow"] = float64(272000)
		client.updateThreadTokenUsage(params)
	}
	if client.lastModelRequest == nil || client.lastModelRequest.InputTokens != 100 || client.lastModelRequest.CacheReadTokens != 50 || client.lastModelRequest.ContextWindow != 272000 {
		t.Fatalf("last request lost or replaced by totals: %+v", client.lastModelRequest)
	}
	replay := codexThreadTokenUsageParams("thread", "previous", map[string]any{"inputTokens": float64(999999)}, map[string]any{"inputTokens": float64(999999)})
	client.updateThreadTokenUsage(replay)
	if client.lastModelRequest.InputSize() != 150 {
		t.Fatal("historical replay replaced the last request")
	}
}

func TestCompactionPointsHonorLowerCLIOverrides(t *testing.T) {
	root := t.TempDir()
	env := []string{"CLAUDE_CONFIG_DIR=" + root, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=50"}
	if got := claudeCompactionTokens(1000000, env, root, nil, "claude-opus-5-5"); got != 500000 {
		t.Fatalf("Claude override = %d", got)
	}
	env = append(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW=200000")
	if got := claudeCompactionTokens(1000000, env, root, nil, "claude-sonnet-4-6"); got != 100000 {
		t.Fatalf("Claude compact window = %d", got)
	}
	if got := claudeCompactionTokens(1000000, env, root, []string{"--settings", "{}"}, "claude-sonnet-4-6"); got != 0 {
		t.Fatal("unverified CLI settings did not fail closed")
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("model_auto_compact_token_limit = 150000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := codexCompactionTokens(272000, root, root, nil, ""); got != 150000 {
		t.Fatalf("Codex config override = %d", got)
	}
	if got := codexCompactionTokens(272000, root, root, []string{"-c", "model_auto_compact_token_limit=100000"}, ""); got != 100000 {
		t.Fatalf("Codex CLI override = %d", got)
	}
	if got := codexCompactionTokens(272000, "", root, nil, ""); got != 0 {
		t.Fatal("unknown Codex config did not fail closed")
	}
}

func TestClaudeCompactionGuardReservesSmallWindowHeadroom(t *testing.T) {
	root := t.TempDir()
	env := []string{"CLAUDE_CONFIG_DIR=" + root}
	if got := claudeCompactionTokens(200000, env, root, nil, "claude-sonnet-4-6"); got != 167000 {
		t.Fatalf("200K conservative guard = %d, want 167000", got)
	}
	env = append(env, "CLAUDE_CODE_DISABLE_1M_CONTEXT=1")
	if got := claudeCompactionTokens(1000000, env, root, nil, "claude-sonnet-4-6"); got != 167000 {
		t.Fatalf("disabled 1M conservative guard = %d, want 167000", got)
	}
	env = append(env, "CLAUDE_CODE_MAX_CONTEXT_TOKENS=120000")
	if got := claudeCompactionTokens(200000, env, root, nil, "claude-sonnet-4-6"); got != 0 {
		t.Fatalf("unverified custom window must fail closed, got %d", got)
	}
}

func TestCodexCompactionGuardHonorsModelCatalogLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("model = 'gpt-test'\nmodel_auto_compact_token_limit = 150000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog := `{"models":[{"slug":"gpt-test","auto_compact_token_limit":50000}]}`
	if err := os.WriteFile(filepath.Join(root, "models_cache.json"), []byte(catalog), 0600); err != nil {
		t.Fatal(err)
	}
	if got := codexCompactionTokens(272000, root, root, nil, ""); got != 50000 {
		t.Fatalf("model catalog compaction guard = %d, want 50000", got)
	}
	if got := codexCompactionTokens(272000, root, root, nil, "unmatched"); got != 0 {
		t.Fatalf("unmatched model must fail closed, got %d", got)
	}
	if got := codexCompactionTokens(272000, root, root, []string{"-c", "model_catalog_json='custom.json'"}, "gpt-test"); got != 0 {
		t.Fatalf("unverified custom catalog must fail closed, got %d", got)
	}
	if got := codexCompactionTokens(272000, root, root, []string{"-c", "model = 'unmatched'"}, ""); got != 0 {
		t.Fatalf("unverified model override must fail closed, got %d", got)
	}
}

func TestCodexCompactionGuardNeedsDefaultPointEvidence(t *testing.T) {
	root := t.TempDir()
	if got := codexCompactionTokens(272000, root, root, nil, "gpt-test"); got != 0 {
		t.Fatalf("unknown default model point must fail closed, got %d", got)
	}
	if err := os.WriteFile(filepath.Join(root, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-test"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := codexCompactionTokens(272000, root, root, nil, "gpt-test"); got != 244800 {
		t.Fatalf("known default catalog point = %d, want 244800", got)
	}
}

func TestCodexCompactionGuardHonorsAttachedProfileFlags(t *testing.T) {
	root := t.TempDir()
	config := "model_auto_compact_token_limit = 150000\n[profiles.low]\nmodel_auto_compact_token_limit = 50000\n"
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-plow", "-p=low", "--profile=low"} {
		if got := codexCompactionTokens(272000, root, root, []string{flag}, ""); got != 50000 {
			t.Fatalf("%s compaction guard = %d, want 50000", flag, got)
		}
	}
}

func TestClaudeCompactionGuardRequiresLegacyExtendedOptIn(t *testing.T) {
	root := t.TempDir()
	env := []string{"CLAUDE_CONFIG_DIR=" + root}
	for _, testCase := range []struct {
		name, model string
		args        []string
		want        int64
	}{
		{"legacy default", "claude-sonnet-4-6", nil, 167000},
		{"legacy dated default", "claude-opus-4-6-20260217", nil, 167000},
		{"explicit extended", "claude-sonnet-4-6", []string{"--model", "sonnet[1m]"}, 950000},
		{"last model flag wins", "claude-sonnet-4-6", []string{"--model", "sonnet[1m]", "--model=sonnet"}, 167000},
		{"native extended", "claude-opus-5-5", nil, 950000},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := claudeCompactionTokens(1000000, env, root, testCase.args, testCase.model); got != testCase.want {
				t.Fatalf("guard = %d, want %d", got, testCase.want)
			}
		})
	}
}

func TestClaudeCompactionGuardHonorsPerModelWindow(t *testing.T) {
	root := t.TempDir()
	env := []string{"CLAUDE_CONFIG_DIR=" + root}
	args := []string{"--model", "sonnet[1m]"}
	settings := `{"modelSettings":{"claude-sonnet-4-6":{"autoCompactWindow":200000},"claude-opus-4-6":{"autoCompactWindow":500000}}}`
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	if got := claudeCompactionTokens(1000000, env, root, args, "claude-sonnet-4-6"); got != 167000 {
		t.Fatalf("per-model guard = %d, want 167000", got)
	}
	if got := claudeCompactionTokens(1000000, env, root, args, "claude-opus-4-6"); got != 467000 {
		t.Fatalf("other model guard = %d, want 467000", got)
	}
	if got := claudeCompactionTokens(1000000, env, root, args, "claude-sonnet-4-6-20260217"); got != 167000 {
		t.Fatalf("dated model guard = %d, want 167000", got)
	}
	env = append(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW=1000000")
	if got := claudeCompactionTokens(1000000, env, root, args, "claude-sonnet-4-6"); got != 167000 {
		t.Fatalf("conservative saved lower limit was raised, got %d", got)
	}
}

func TestClaudeLastRequestExcludesTotalsAndSubagents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake CLI")
	}
	fakePath := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
IFS= read -r input
printf '%s\n' '{"type":"assistant","message":{"id":"first","model":"claude-sonnet-4-6","usage":{"input_tokens":1000,"cache_read_input_tokens":120000},"content":[{"type":"text","text":"first"}]}}'
printf '%s\n' '{"type":"assistant","message":{"id":"last","model":"claude-sonnet-4-6","usage":{"input_tokens":2000,"cache_read_input_tokens":160000,"cache_creation_input_tokens":300},"content":[{"type":"text","text":"done"}]}}'
printf '%s\n' '{"type":"assistant","parent_tool_use_id":"subagent","message":{"id":"sub","model":"claude-haiku-4-5","usage":{"input_tokens":900000},"content":[{"type":"text","text":"subagent"}]}}'
printf '%s\n' '{"type":"result","is_error":false,"session_id":"fresh","result":"done","modelUsage":{"claude-sonnet-4-6":{"inputTokens":900000,"contextWindow":1000000}}}'
`
	writeTestExecutable(t, fakePath, []byte(script))
	backend, err := New("claude", Config{ExecutablePath: fakePath, Env: map[string]string{"IS_SANDBOX": "1", "CLAUDE_CONFIG_DIR": t.TempDir()}, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "test prompt", ExecOptions{Model: "claude-sonnet-4-6[1m]", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	last := result.LastModelRequest
	if last == nil || last.InputTokens != 2000 || last.CacheReadTokens != 160000 || last.CacheWriteTokens != 300 || last.ContextWindow != 1000000 || last.CLICompactionTokens != 950000 {
		t.Fatalf("last request missing or cumulative/subagent usage was selected: %+v", last)
	}
}
