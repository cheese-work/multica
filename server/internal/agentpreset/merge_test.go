package agentpreset

import (
	"reflect"
	"testing"
)

// I4: a preset that sets context args replaces only the allow-listed context
// args; every other agent arg keeps its bytes and order.
func TestMergeContextArgs(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		custom   []string
		ctx      []string
		want     []string
	}{
		{"nil ctx leaves custom untouched", "claude", []string{"--autocompact", "100k", "--add-dir", "/x"}, nil, []string{"--autocompact", "100k", "--add-dir", "/x"}},
		{"claude replaces autocompact, keeps order", "claude",
			[]string{"--sandbox", "workspace-write", "--autocompact", "100k", "--add-dir", "/x"},
			[]string{"--autocompact", "200k"},
			[]string{"--sandbox", "workspace-write", "--add-dir", "/x", "--autocompact", "200k"}},
		{"claude equals form is stripped", "claude",
			[]string{"--autocompact=100k", "--verbose"}, []string{"--autocompact", "200k"},
			[]string{"--verbose", "--autocompact", "200k"}},
		{"codex strips only the two context keys", "codex",
			[]string{"-c", "sandbox_mode=workspace-write", "-c", "model_context_window=100000", "-c", "model_auto_compact_token_limit=90000", "-c", "model_reasoning_effort=high"},
			[]string{"-c", "model_context_window=200000"},
			[]string{"-c", "sandbox_mode=workspace-write", "-c", "model_reasoning_effort=high", "-c", "model_context_window=200000"}},
		{"empty non-nil ctx only strips", "claude",
			[]string{"--autocompact", "100k", "--verbose"}, []string{},
			[]string{"--verbose"}},
		{"no custom args", "claude", nil, []string{"--autocompact", "200k"}, []string{"--autocompact", "200k"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MergeContextArgs(c.provider, c.custom, c.ctx); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("MergeContextArgs = %q, want %q", got, c.want)
			}
		})
	}
}

// F1 (CHE-2246 review): every spelling of an inherited codex context override
// is cleared by an explicit empty preset and recorded in the claim snapshot;
// unrelated config overrides keep their bytes.
func TestCodexContextArgSpellings(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"short split", []string{"-c", "model_context_window=100000"}},
		{"long split", []string{"--config", "model_context_window=100000"}},
		{"long inline", []string{"--config=model_context_window=100000"}},
		{"short inline", []string{"-c=model_context_window=100000"}},
		{"toml whitespace", []string{"-c", "model_context_window = 100000"}},
		{"auto compact long inline", []string{"--config=model_auto_compact_token_limit=90000"}},
	}
	keep := []string{"--sandbox", "workspace-write", "--config", "model_reasoning_effort=high", "-c=sandbox_mode=x"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			custom := append(append([]string{}, keep...), c.args...)
			if got := MergeContextArgs("codex", custom, []string{}); !reflect.DeepEqual(got, keep) {
				t.Errorf("explicit clear = %q, want %q", got, keep)
			}
			if got := ContextArgsOf("codex", custom); !reflect.DeepEqual(got, c.args) {
				t.Errorf("snapshot = %q, want %q", got, c.args)
			}
		})
	}
}
