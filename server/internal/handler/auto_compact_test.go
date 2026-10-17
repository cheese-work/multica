package handler

import "testing"

// Retrospective test (added with the implementation, not first): precedence is
// agent override over workspace over the built-in defaults, field by field.
func TestResolveAutoCompactPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, workspace, agent string
		want                   autoCompact
	}{
		{"defaults", `{}`, `{}`, autoCompact{true, 80}},
		{"workspace", `{"auto_compact":{"enabled":false,"threshold_pct":60}}`, `{}`, autoCompact{false, 60}},
		{"workspace partial", `{"auto_compact":{"threshold_pct":70}}`, `{}`, autoCompact{true, 70}},
		{"agent wins per field", `{"auto_compact":{"enabled":false,"threshold_pct":60}}`, `{"auto_compact":{"threshold_pct":85}}`, autoCompact{false, 85}},
		{"agent re-enables", `{"auto_compact":{"enabled":false}}`, `{"auto_compact":{"enabled":true}}`, autoCompact{true, 80}},
		{"agent null inherits", `{"auto_compact":{"threshold_pct":55}}`, `{"auto_compact":null}`, autoCompact{true, 55}},
		{"stored out-of-range value is ignored", `{"auto_compact":{"threshold_pct":5}}`, `{}`, autoCompact{true, 80}},
		{"unparsable layer is skipped", `not json`, `{"auto_compact":{"threshold_pct":66}}`, autoCompact{true, 66}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveAutoCompact([]byte(tc.workspace), []byte(tc.agent)); got != tc.want {
				t.Fatalf("resolveAutoCompact = %+v, want %+v", got, tc.want)
			}
		})
	}
}
