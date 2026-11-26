package handler

import (
	"encoding/json"
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

// CHE-10000 (S2): the DM run caps validate like the thresholds, with their own
// ranges, and null is accepted as "inherit" so a revert card can clear one.
func TestValidateAutoCompactRunCapFields(t *testing.T) {
	cfg := func(key string, v any) map[string]any {
		return map[string]any{"auto_compact": map[string]any{key: v}}
	}
	for _, tc := range []struct {
		key  string
		v    any
		want bool
	}{
		{"dm_run_cap_tokens_1m", float64(100_000), true},
		{"dm_run_cap_tokens_1m", float64(430_000), true},
		{"dm_run_cap_tokens_1m", float64(1_000_000), true},
		{"dm_run_cap_tokens_1m", float64(1_000_001), false},
		{"dm_run_cap_tokens_1m", float64(99_999), false},
		{"dm_run_cap_tokens_1m", float64(430_000.5), false},
		{"dm_run_cap_tokens_1m", "430000", false},
		{"dm_run_cap_tokens_1m", true, false},
		{"dm_run_cap_tokens_1m", nil, true},
		{"dm_run_cap_tokens_200k", float64(100_000), true},
		{"dm_run_cap_tokens_200k", float64(190_000), true},
		{"dm_run_cap_tokens_200k", float64(200_000), true},
		{"dm_run_cap_tokens_200k", float64(200_001), false},
		{"dm_run_cap_tokens_200k", float64(99_999), false},
		{"dm_run_cap_tokens_200k", nil, true},
	} {
		err := validateAutoCompactConfig(cfg(tc.key, tc.v))
		if (err == nil) != tc.want {
			t.Errorf("%s=%v: err=%v, want valid=%v", tc.key, tc.v, err, tc.want)
		}
		if err != nil && !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%v: error %q does not name the field", tc.key, tc.v, err)
		}
	}
	// The ordinary PATCH body check takes the same keys, null included.
	if err := validateAutoCompactPatch(map[string]any{"dm_run_cap_tokens_1m": nil}); err != nil {
		t.Errorf("PATCH null run cap: %v", err)
	}
	if err := validateAutoCompactPatch(map[string]any{"dm_run_cap_tokens_1m": float64(2_000_000)}); err == nil {
		t.Error("PATCH out-of-range run cap was accepted")
	}
	// A cap-only patch must not null a saved percent: a cap is not a threshold.
	patch := clearStalePct([]byte(`{"dm_run_cap_tokens_1m":430000}`))
	if string(patch) != `{"dm_run_cap_tokens_1m":430000}` {
		t.Errorf("clearStalePct changed a cap-only patch: %s", patch)
	}
}

// The decision-action settings patch (the passkey card path) validates
// auto_compact with the same validator, so it takes the new keys and refuses
// out-of-range values; the revert card's null is accepted.
func TestSettingsPatchAcceptsRunCapKeys(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{"both thresholds and cap", `{"threshold_tokens_1m":350000,"dm_run_cap_tokens_1m":430000}`, true},
		{"200k cap", `{"dm_run_cap_tokens_200k":190000}`, true},
		{"revert card clears the cap", `{"threshold_tokens_1m":400000,"dm_run_cap_tokens_1m":null}`, true},
		{"cap above 1M", `{"dm_run_cap_tokens_1m":1000001}`, false},
		{"200k cap above 200k", `{"dm_run_cap_tokens_200k":250000}`, false},
		{"cap below 100k", `{"dm_run_cap_tokens_1m":50000}`, false},
		{"cap as string", `{"dm_run_cap_tokens_1m":"430000"}`, false},
	} {
		err := validateSettingsPatchValue("auto_compact", json.RawMessage(tc.value))
		if (err == nil) != tc.want {
			t.Errorf("%s: err=%v, want valid=%v", tc.name, err, tc.want)
		}
	}
}
