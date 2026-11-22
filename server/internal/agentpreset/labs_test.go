package agentpreset

import "testing"

func TestLabsOfReadsProviderSlashModel(t *testing.T) {
	labs := Labs{"claude/claude-sonnet-5-5": "anthropic", "codex/gpt-5.5": "openai"}
	for _, tc := range []struct{ provider, model, want string }{
		{"claude", "claude-sonnet-5-5", "anthropic"},
		{"codex", "gpt-5.5", "openai"},
		{"codex", "unlisted", ""},
		{"", "", ""},
	} {
		if got := labs.Of(tc.provider, tc.model); got != tc.want {
			t.Errorf("Of(%q, %q) = %q, want %q", tc.provider, tc.model, got, tc.want)
		}
	}
	var none Labs
	if got := none.Of("claude", "x"); got != "" {
		t.Errorf("nil Labs = %q, want empty (no lock)", got)
	}
}
