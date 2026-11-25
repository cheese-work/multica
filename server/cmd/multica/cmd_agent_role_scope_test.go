package main

import "testing"

func TestRoleScopeCell(t *testing.T) {
	if got := roleScopeCell(map[string]any{}); got != "all" {
		t.Errorf("no role_scope = %q, want all", got)
	}
	if got := roleScopeCell(map[string]any{"role_scope": []any{"plan", "decision"}}); got != "plan,decision" {
		t.Errorf("role_scope = %q, want plan,decision", got)
	}
}
