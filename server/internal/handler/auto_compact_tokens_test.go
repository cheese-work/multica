package handler

import (
	"strings"
	"testing"
)

// Retrospective test (added with the implementation, not first): CHE-4331
// token thresholds validate without a database.
func TestValidateAutoCompactTokenFields(t *testing.T) {
	cfg := func(key string, v any) map[string]any {
		return map[string]any{"auto_compact": map[string]any{key: v}}
	}
	for _, tc := range []struct {
		key  string
		v    any
		want bool
	}{
		{"threshold_tokens_200k", float64(100_000), true},
		{"threshold_tokens_200k", float64(200_000), true},
		{"threshold_tokens_200k", float64(200_001), false},
		{"threshold_tokens_200k", float64(99_999), false},
		{"threshold_tokens_1m", float64(400_000), true},
		{"threshold_tokens_1m", float64(400_001), false},
		{"threshold_tokens_1m", float64(250_000.5), false},
		{"threshold_tokens_1m", "200000", false},
		{"threshold_tokens_1m", nil, false},
	} {
		err := validateAutoCompactConfig(cfg(tc.key, tc.v))
		if (err == nil) != tc.want {
			t.Errorf("%s=%v: err=%v, want valid=%v", tc.key, tc.v, err, tc.want)
		}
		if err != nil && !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%v: error %q does not name the field", tc.key, tc.v, err)
		}
	}
}
