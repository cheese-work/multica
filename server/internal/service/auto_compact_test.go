package service

import "testing"

func pctRule(enabled bool, pct int) AutoCompact {
	return AutoCompact{Enabled: enabled, ThresholdPct: pct, Tokens200k: 190_000, Tokens1M: 200_000}
}

func tokenRule(enabled bool, small, large int64) AutoCompact {
	return AutoCompact{Enabled: enabled, Tokens200k: small, Tokens1M: large}
}

// Retrospective test (added with the implementation, not first): precedence is
// agent override over workspace over the built-in defaults, field by field.
func TestResolveAutoCompactPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, workspace, agent string
		want                   AutoCompact
	}{
		{"defaults are per-window tokens", `{}`, `{}`, tokenRule(true, 190_000, 200_000)},
		{"workspace", `{"auto_compact":{"enabled":false,"threshold_pct":60}}`, `{}`, pctRule(false, 60)},
		{"workspace partial", `{"auto_compact":{"threshold_pct":70}}`, `{}`, pctRule(true, 70)},
		{"agent wins per field", `{"auto_compact":{"enabled":false,"threshold_pct":60}}`, `{"auto_compact":{"threshold_pct":85}}`, pctRule(false, 85)},
		{"agent re-enables", `{"auto_compact":{"enabled":false}}`, `{"auto_compact":{"enabled":true}}`, tokenRule(true, 190_000, 200_000)},
		{"agent null inherits", `{"auto_compact":{"threshold_pct":55}}`, `{"auto_compact":null}`, pctRule(true, 55)},
		{"stored out-of-range value is ignored", `{"auto_compact":{"threshold_pct":5}}`, `{}`, tokenRule(true, 190_000, 200_000)},
		{"workspace tokens", `{"auto_compact":{"threshold_tokens_200k":150000,"threshold_tokens_1m":300000}}`, `{}`, tokenRule(true, 150_000, 300_000)},
		{"token field out of range is ignored", `{"auto_compact":{"threshold_tokens_200k":10,"threshold_tokens_1m":900000}}`, `{}`, tokenRule(true, 190_000, 200_000)},
		{"agent pct replaces workspace tokens", `{"auto_compact":{"threshold_tokens_1m":300000}}`, `{"auto_compact":{"threshold_pct":60}}`, AutoCompact{Enabled: true, ThresholdPct: 60, Tokens200k: 190_000, Tokens1M: 300_000}},
		{"agent tokens replace workspace pct", `{"auto_compact":{"threshold_pct":60}}`, `{"auto_compact":{"threshold_tokens_1m":250000}}`, tokenRule(true, 190_000, 250_000)},
		{"unparsable layer is skipped", `not json`, `{"auto_compact":{"threshold_pct":66}}`, pctRule(true, 66)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveAutoCompact([]byte(tc.workspace), []byte(tc.agent))
			got.Tokens1MSet = false // covered by TestClaudeCompactEnv
			if got != tc.want {
				t.Fatalf("ResolveAutoCompact = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAutoCompactReachedIsExactAtTheBoundary(t *testing.T) {
	rule := AutoCompact{Enabled: true, ThresholdPct: 80}
	for _, tc := range []struct {
		tokens, window int64
		want           bool
	}{
		{159_999, 200_000, false}, {160_000, 200_000, true}, {799_999, 1_000_000, false}, {800_000, 1_000_000, true},
		{1, 0, false}, {0, 200_000, false},
	} {
		if got := rule.Reached(tc.tokens, tc.window); got != tc.want {
			t.Errorf("Reached(%d, %d) = %v, want %v", tc.tokens, tc.window, got, tc.want)
		}
	}
	if (AutoCompact{Enabled: false, ThresholdPct: 50}).Reached(190_000, 200_000) {
		t.Error("a disabled rule must never be reached")
	}
}

// CHE-4331: with no percent saved, the threshold is a token count chosen by
// the model's window: 190k for windows up to 200k, 200k for larger ones.
func TestAutoCompactReachedByWindowTokens(t *testing.T) {
	rule := tokenRule(true, 190_000, 200_000)
	for _, tc := range []struct {
		tokens, window int64
		want           bool
	}{
		{189_999, 200_000, false}, {190_000, 200_000, true},
		{199_999, 1_000_000, false}, {200_000, 1_000_000, true},
		{121_599, 128_000, false}, {121_600, 128_000, true}, // small window: 95% cap
		{1, 0, false}, {0, 1_000_000, false},
	} {
		if got := rule.Reached(tc.tokens, tc.window); got != tc.want {
			t.Errorf("Reached(%d, %d) = %v, want %v", tc.tokens, tc.window, got, tc.want)
		}
	}
	if tokenRule(false, 190_000, 200_000).Reached(900_000, 1_000_000) {
		t.Error("a disabled rule must never be reached")
	}
}

// CHE-4438: the notice states the trigger size and limit once, never says the
// DM changed, and carries no percent.
func TestAutoCompactNotice(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tokens, win int64
		rule        AutoCompact
		want        string
	}{
		{"token rule on a 1M window", 213_000, 1_000_000, tokenRule(true, 190_000, 200_000), "Compacted in this DM at 213k tokens (limit 200k). Earlier messages are summarized below."},
		{"percent rule", 164_000, 200_000, pctRule(true, 80), "Compacted in this DM at 164k tokens (limit 160k). Earlier messages are summarized below."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoCompactNotice(tc.tokens, tc.win, tc.rule); got != tc.want {
				t.Fatalf("autoCompactNotice = %q, want %q", got, tc.want)
			}
		})
	}
}
