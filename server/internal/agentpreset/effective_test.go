package agentpreset

import (
	"reflect"
	"strings"
	"testing"
)

var labs = Labs{
	"claude/claude-opus-5-5":   "anthropic",
	"claude/claude-sonnet-5-5": "anthropic",
	"claude/claude-gamma-1":    "other",
	"codex/gpt-6.1-sol":        "openai",
}

func TestEffective(t *testing.T) {
	autocompact := []string{"--autocompact", "200k"}
	tests := []struct {
		name       string
		agent      Agent
		preset     Preset
		wantReason string
		want       Settings
	}{
		{
			name:   "valid preset merges over the agent row",
			agent:  claudeAgent,
			preset: Preset{Name: "p", ThinkingLevel: sp("low"), ContextArgs: autocompact},
			want:   Settings{Model: "claude-opus-5-5", ThinkingLevel: "low", ContextArgs: autocompact},
		},
		{
			name:       "E1 rebind Claude to Codex leaves --autocompact: stale",
			agent:      Agent{Provider: "codex", Model: "gpt-6.1-sol"},
			preset:     Preset{Name: "p", ContextArgs: autocompact},
			wantReason: ReasonContextArgs,
		},
		{
			name:   "pinned same-lab model is valid",
			agent:  Agent{Provider: "claude", Model: "claude-opus-5-5"},
			preset: Preset{Name: "p", Model: sp("claude-sonnet-5-5")},
			want:   Settings{Model: "claude-sonnet-5-5"},
		},
		{
			name:       "E2 pinned old-lab model after the base moves to another lab: lab lock",
			agent:      Agent{Provider: "claude", Model: "claude-gamma-1"},
			preset:     Preset{Name: "p", Model: sp("claude-sonnet-5-5")},
			wantReason: ReasonLabLock,
		},
		{
			name:       "pinned model not listed while the base is listed: lab lock",
			agent:      Agent{Provider: "claude", Model: "claude-opus-5-5"},
			preset:     Preset{Name: "p", Model: sp("claude-other-lab")},
			wantReason: ReasonLabLock,
		},
		{
			name:   "E3a inherited model, base moves within the lab: claimed model follows the base",
			agent:  Agent{Provider: "claude", Model: "claude-sonnet-5-5"},
			preset: Preset{Name: "p", ThinkingLevel: sp("low")},
			want:   Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "low"},
		},
		{
			name:   "E3b inherited model, base moves to another lab: valid, run lab follows the base",
			agent:  Agent{Provider: "codex", Model: "gpt-6.1-sol"},
			preset: Preset{Name: "p"},
			want:   Settings{Model: "gpt-6.1-sol"},
		},
		{
			name:       "E4 thinking level invalid for the new provider enum: stale",
			agent:      Agent{Provider: "claude", Model: "claude-opus-5-5"},
			preset:     Preset{Name: "p", ThinkingLevel: sp("not-a-level")},
			wantReason: ReasonThinkingLevel,
		},
		{
			name:       "E6 tier-only preset after rebind to Claude: stale",
			agent:      Agent{Provider: "claude", Model: "claude-opus-5-5"},
			preset:     Preset{Name: "p", ServiceTier: sp("fast")},
			wantReason: ReasonServiceTier,
		},
		{
			name:   "tier-only preset on Codex is valid",
			agent:  Agent{Provider: "codex", Model: "gpt-6.1-sol"},
			preset: Preset{Name: "p", ServiceTier: sp("fast")},
			want:   Settings{Model: "gpt-6.1-sol", ServiceTier: "fast"},
		},
		{
			name:       "pinned Claude model on a Codex agent: model incompatible",
			agent:      Agent{Provider: "codex", Model: "gpt-6.1-sol"},
			preset:     Preset{Name: "p", Model: sp("claude-opus-5-5")},
			wantReason: ReasonModel,
		},
		{
			name:   "no lab lock when the base model is not listed",
			agent:  Agent{Provider: "claude", Model: "claude-unlisted-1"},
			preset: Preset{Name: "p", Model: sp("claude-other-lab")},
			want:   Settings{Model: "claude-other-lab"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Effective(tc.agent, labs, tc.preset)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if reason == "" && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("settings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestValidateContextArgs(t *testing.T) {
	ok := []struct {
		provider string
		args     []string
	}{
		{"claude", nil},
		{"claude", []string{"--autocompact", "200k"}},
		{"claude", []string{"--autocompact", "1m"}},
		{"codex", []string{"-c", "model_context_window=200000", "-c", "model_auto_compact_token_limit=180000"}},
		{"codex", []string{"-c", "model_context_window=200000"}},
	}
	for _, tc := range ok {
		if err := ValidateContextArgs(tc.provider, tc.args); err != nil {
			t.Errorf("%s %v: unexpected error %v", tc.provider, tc.args, err)
		}
	}
	bad := []struct {
		provider string
		args     []string
	}{
		{"claude", []string{"--dangerously-skip-permissions"}},
		{"claude", []string{"--add-dir", "/tmp"}},
		{"claude", []string{"--mcp-config", "x.json"}},
		{"claude", []string{"--permission-mode", "bypassPermissions"}},
		{"claude", []string{"--model", "claude-opus-5-5"}},
		{"claude", []string{"-m", "x"}},
		{"claude", []string{"--autocompact"}},
		{"claude", []string{"--autocompact", "huge"}},
		{"claude", []string{"--autocompact", "200k", "--autocompact", "1m"}},
		{"claude", []string{"--autocompact=200k"}},
		{"codex", []string{"-c", "sandbox_mode=danger-full-access"}},
		{"codex", []string{"-c", "sandbox_workspace_write.writable_roots=[\"/\"]"}},
		{"codex", []string{"-c", "model_context_window=abc"}},
		{"codex", []string{"-c", "model_context_window=1", "-c", "model_context_window=2"}},
		{"codex", []string{"--sandbox", "danger-full-access"}},
		{"codex", []string{"-c"}},
		{"gemini", []string{"--autocompact", "200k"}},
		{"opencode", []string{"-c", "model_context_window=1"}},
	}
	for _, tc := range bad {
		if err := ValidateContextArgs(tc.provider, tc.args); err == nil {
			t.Errorf("%s %v: expected error", tc.provider, tc.args)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, n := range []string{"200k", "fast", "a", "a-b-c", strings.Repeat("a", 32)} {
		if err := ValidateName(n); err != nil {
			t.Errorf("%q: unexpected error %v", n, err)
		}
	}
	for _, n := range []string{"", BaseName, "reset", "-a", "A", "a_b", "a b", strings.Repeat("a", 33), "a/b"} {
		if err := ValidateName(n); err == nil {
			t.Errorf("%q: expected error", n)
		}
	}
}

func TestValidate(t *testing.T) {
	// I6 lab lock at create.
	err := Validate(
		Agent{Provider: "claude", Model: "claude-opus-5-5"}, labs,
		Preset{Name: "p", Model: sp("claude-other-lab")},
	)
	if err == nil || !strings.Contains(err.Error(), ReasonLabLock) {
		t.Fatalf("lab lock at create: err = %v", err)
	}
	if err := Validate(claudeAgent, labs, Preset{Name: "ok", ThinkingLevel: sp("low")}); err != nil {
		t.Fatalf("valid preset rejected: %v", err)
	}
	if err := Validate(claudeAgent, labs, Preset{Name: BaseName}); err == nil {
		t.Fatal("reserved name accepted")
	}
	// I2 sandbox args at create.
	err = Validate(claudeAgent, labs, Preset{Name: "p", ContextArgs: []string{"--dangerously-skip-permissions"}})
	if err == nil {
		t.Fatal("sandbox arg accepted")
	}
}

// NULL context_args inherit the agent's own args; an explicit empty list
// replaces them, so the claim merger can remove existing context overrides.
func TestEffectiveExplicitEmptyContextArgsIsNotInherit(t *testing.T) {
	inherit, _ := Effective(claudeAgent, labs, Preset{Name: "p"})
	if inherit.ContextArgs != nil {
		t.Fatalf("NULL context args = %#v, want nil", inherit.ContextArgs)
	}
	clear, reason := Effective(claudeAgent, labs, Preset{Name: "p", ContextArgs: []string{}})
	if reason != "" {
		t.Fatalf("empty context args stale: %s", reason)
	}
	if clear.ContextArgs == nil || len(clear.ContextArgs) != 0 {
		t.Fatalf("explicit [] = %#v, want non-nil empty", clear.ContextArgs)
	}
}
