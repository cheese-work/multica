package service

import (
	"reflect"
	"testing"
)

// CHE-4331 Part 2 and CHE-10000 (S2): the claim hands a Claude Code run the
// compaction env the saved auto_compact setting implies; nothing is written to
// agent env. The run cap is its own pair of keys: a chat task gets
// max(dm_run_cap, DM threshold); any other task gets the built-in cap. Neither
// reads threshold_pct or the threshold token keys for the cap itself.
func TestClaudeCompactEnv(t *testing.T) {
	ws := func(s string) []byte { return []byte(`{"auto_compact":` + s + `}`) }
	const window, pct = "CLAUDE_CODE_AUTO_COMPACT_WINDOW", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"
	both := `{"threshold_tokens_1m":350000,"dm_run_cap_tokens_1m":430000}`
	for _, tc := range []struct {
		name             string
		provider, model  string
		chat             bool
		workspace, agent []byte
		userEnv          map[string]string
		want             map[string]string
	}{
		{"chat 1m, saved threshold and cap", "claude", "opus[1m]", true, ws(both), nil, nil, map[string]string{window: "430000"}},
		{"issue 1m, same settings stays at the built-in cap", "claude", "opus[1m]", false, ws(both), nil, nil, map[string]string{window: "400000"}},
		{"chat 1m, no saved keys", "claude", "claude-opus-4-8[1m]", true, nil, nil, nil, map[string]string{window: "400000"}},
		{"issue 1m, no saved keys", "claude", "claude-opus-4-8[1m]", false, nil, nil, nil, map[string]string{window: "400000"}},
		{"chat 1m, cap below threshold gives the threshold", "claude", "opus[1m]", true,
			ws(`{"threshold_tokens_1m":350000,"dm_run_cap_tokens_1m":300000}`), nil, nil, map[string]string{window: "350000"}},
		{"chat 1m, threshold equal to the default cap", "claude", "opus[1m]", true,
			ws(`{"threshold_tokens_1m":400000}`), nil, nil, map[string]string{window: "400000"}},
		{"chat 1m, cap only", "claude", "opus[1m]", true, ws(`{"dm_run_cap_tokens_1m":500000}`), nil, nil, map[string]string{window: "500000"}},
		{"issue 1m, cap only", "claude", "opus[1m]", false, ws(`{"dm_run_cap_tokens_1m":500000}`), nil, nil, map[string]string{window: "400000"}},
		{"chat 1m, saved percent leaves the env unchanged", "claude", "opus[1m]", true, ws(`{"threshold_pct":80}`), nil, nil, map[string]string{window: "400000"}},
		{"issue 1m, saved percent leaves the env unchanged", "claude", "opus[1m]", false, ws(`{"threshold_pct":80}`), nil, nil, map[string]string{window: "400000"}},
		{"issue 1m, saved low threshold does not lower the cap", "claude", "opus[1m]", false, ws(`{"threshold_tokens_1m":150000}`), nil, nil, map[string]string{window: "400000"}},
		{"agent cap overrides workspace cap", "claude", "opus[1m]", true, ws(both), ws(`{"dm_run_cap_tokens_1m":450000}`), nil, map[string]string{window: "450000"}},
		{"agent null cap inherits the workspace cap", "claude", "opus[1m]", true, ws(both), ws(`{"dm_run_cap_tokens_1m":null}`), nil, map[string]string{window: "430000"}},
		{"chat 200k, defaults", "claude", "claude-sonnet-5-5", true, nil, nil, nil, map[string]string{pct: "95"}},
		{"issue 200k, defaults", "claude", "claude-sonnet-5-5", false, nil, nil, nil, map[string]string{pct: "95"}},
		{"chat 200k, saved cap", "claude", "sonnet", true, ws(`{"dm_run_cap_tokens_200k":150000}`), nil, nil, map[string]string{pct: "95"}},
		{"chat 200k, cap above threshold", "claude", "sonnet", true,
			ws(`{"threshold_tokens_200k":120000,"dm_run_cap_tokens_200k":180000}`), nil, nil, map[string]string{pct: "90"}},
		{"issue 200k, saved cap and threshold", "claude", "sonnet", false,
			ws(`{"threshold_tokens_200k":120000,"dm_run_cap_tokens_200k":180000}`), nil, nil, map[string]string{pct: "95"}},
		{"chat 200k, threshold above cap", "claude", "sonnet", true,
			ws(`{"threshold_tokens_200k":150000,"dm_run_cap_tokens_200k":100000}`), nil, nil, map[string]string{pct: "75"}},
		{"chat 200k, saved percent leaves the env unchanged", "claude", "sonnet", true, ws(`{"threshold_pct":80}`), nil, nil, map[string]string{pct: "95"}},
		{"user env wins (200k)", "claude", "sonnet", true, nil, nil, map[string]string{"claude_autocompact_pct_override": "70"}, nil},
		{"user env wins (1m)", "claude", "opus[1m]", true, ws(both), nil, map[string]string{"Claude_Code_Auto_Compact_Window": "250000"}, nil},
		{"disabled", "claude", "opus[1m]", true, ws(`{"enabled":false}`), nil, nil, nil},
		{"non-claude runtime", "codex", "gpt-5", true, ws(both), nil, nil, nil},
	} {
		got := ClaudeCompactEnv(tc.provider, tc.model, tc.chat, tc.workspace, tc.agent, tc.userEnv)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
