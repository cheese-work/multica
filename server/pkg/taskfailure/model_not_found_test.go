package taskfailure

import "testing"

// claudeCodeSelectedModelErr is the text Claude Code 2.1.318 / 2.1.329 prints
// for an HTTP 404 model_not_found. It contains "you may not have access", which
// rule 3 (auth) used to claim before rule 8 (model not found) could run.
const claudeCodeSelectedModelErr = "There's an issue with the selected model (claude-opus-4-8). " +
	"It may not exist or you may not have access to it. Run --model to pick a different model."

func TestClassifyClaudeCodeSelectedModel(t *testing.T) {
	for name, raw := range map[string]string{
		"full text":         claudeCodeSelectedModelErr,
		"curly apostrophe":  "There’s an issue with the selected model (claude-opus-4-8). It may not exist or you may not have access to it.",
		"without prefix":    "It may not exist or you may not have access to it. Run --model to pick a different model.",
		"wrapped in output": "claude returned an error: " + claudeCodeSelectedModelErr,
	} {
		if got := Classify(raw); got != ReasonAgentModelNotFoundOrUnavailable {
			t.Errorf("%s: Classify = %s, want %s", name, got, ReasonAgentModelNotFoundOrUnavailable)
		}
	}
}

// A bare "you may not have access" without Claude Code's model shape stays an
// auth/access failure.
func TestClassifyBareMayNotHaveAccessStaysAuth(t *testing.T) {
	if got := Classify("you may not have access to claude-3-opus"); got != ReasonAgentProviderAuthOrAccess {
		t.Errorf("Classify = %s, want %s", got, ReasonAgentProviderAuthOrAccess)
	}
}

func TestNormalizeDaemonReasonUpgradesClaudeCodeSelectedModel(t *testing.T) {
	for _, legacy := range []string{
		string(ReasonAgentProviderAuthOrAccess),
		string(ReasonAgentUnknown),
		"agent_error",
	} {
		if got := NormalizeDaemonReason(legacy, claudeCodeSelectedModelErr); got != ReasonAgentModelNotFoundOrUnavailable {
			t.Errorf("NormalizeDaemonReason(%q) = %s, want %s", legacy, got, ReasonAgentModelNotFoundOrUnavailable)
		}
	}
	// Narrow: a real auth failure and an unrelated reason are untouched.
	if got := NormalizeDaemonReason(string(ReasonAgentProviderAuthOrAccess), "you may not have access to claude-3-opus"); got != ReasonAgentProviderAuthOrAccess {
		t.Errorf("bare access text: got %s, want auth preserved", got)
	}
	if got := NormalizeDaemonReason(string(ReasonAgentProviderNetwork), claudeCodeSelectedModelErr); got != ReasonAgentProviderNetwork {
		t.Errorf("unrelated reason: got %s, want preserved", got)
	}
}
