package handler

import "testing"

func TestValidateBlockedDispatchDedupSetting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings any
		wantErr  bool
	}{
		{"unset uses default", map[string]any{}, false},
		{"block", map[string]any{"blocked_dispatch_dedup": map[string]any{"mode": "block"}}, false},
		{"warn", map[string]any{"blocked_dispatch_dedup": map[string]any{"mode": "warn"}}, false},
		{"off", map[string]any{"blocked_dispatch_dedup": map[string]any{"mode": "off"}}, false},
		{"invalid mode", map[string]any{"blocked_dispatch_dedup": map[string]any{"mode": "skip"}}, true},
		{"unknown option", map[string]any{"blocked_dispatch_dedup": map[string]any{"mode": "off", "scope": "issue"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBlockedDispatchDedupSetting(tc.settings)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateBlockedDispatchDedupSetting() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
