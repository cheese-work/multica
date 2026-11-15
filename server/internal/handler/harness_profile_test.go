package handler

import "testing"

func TestValidateHarnessProfileConfig(t *testing.T) {
	ok := []any{
		nil,
		map[string]any{},
		map[string]any{"harness_profile": nil},
		map[string]any{"harness_profile": "personal"},
		map[string]any{"harness_profile": "managed"},
	}
	for _, v := range ok {
		if err := validateHarnessProfileConfig(v); err != nil {
			t.Errorf("%v: unexpected error %v", v, err)
		}
	}
	bad := []any{
		map[string]any{"harness_profile": "managed_sandbox"},
		map[string]any{"harness_profile": "nope"},
		map[string]any{"harness_profile": 1.0},
		map[string]any{"harness_profile": true},
		map[string]any{"harness_profile": ""},
	}
	for _, v := range bad {
		if err := validateHarnessProfileConfig(v); err == nil {
			t.Errorf("%v: expected error", v)
		}
	}
}
