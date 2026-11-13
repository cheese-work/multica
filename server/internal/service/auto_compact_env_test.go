package service

import (
	"reflect"
	"testing"
)

// CHE-4331 Part 2: the claim hands a Claude Code run the compaction env the
// saved auto_compact setting implies; nothing is written to agent env.
func TestClaudeCompactEnv(t *testing.T) {
	ws := func(s string) []byte { return []byte(`{"auto_compact":` + s + `}`) }
	for _, tc := range []struct {
		name             string
		provider, model  string
		workspace, agent []byte
		userEnv          map[string]string
		want             map[string]string
	}{
		{"1m model, defaults", "claude", "claude-opus-4-8[1m]", nil, nil, nil,
			map[string]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "400000"}},
		{"200k model, defaults", "claude", "claude-sonnet-5-5", nil, nil, nil,
			map[string]string{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "95"}},
		{"workspace 1m tokens", "claude", "opus[1m]", ws(`{"threshold_tokens_1m":300000}`), nil, nil,
			map[string]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "300000"}},
		{"agent overrides workspace", "claude", "opus[1m]", ws(`{"threshold_tokens_1m":300000}`), ws(`{"threshold_tokens_1m":250000}`), nil,
			map[string]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "250000"}},
		{"200k tokens to percent", "claude", "sonnet", ws(`{"threshold_tokens_200k":150000}`), nil, nil,
			map[string]string{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "75"}},
		{"saved percent on 200k model", "claude", "sonnet", ws(`{"threshold_pct":80}`), nil, nil,
			map[string]string{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "80"}},
		{"user env wins", "claude", "sonnet", nil, nil, map[string]string{"claude_autocompact_pct_override": "70"}, nil},
		{"disabled", "claude", "opus[1m]", ws(`{"enabled":false}`), nil, nil, nil},
		{"non-claude runtime", "codex", "gpt-5", nil, nil, nil, nil},
	} {
		got := ClaudeCompactEnv(tc.provider, tc.model, tc.workspace, tc.agent, tc.userEnv)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
