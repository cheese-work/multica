package issueworkflow

import (
	"strings"
	"testing"
)

// CHE-9075 (C1, R12): quick_create_discovery is off unless a workspace sets
// "on"; the server keeps a requested Discovery status only on a workflow that
// lists the step.

func TestQuickCreateDiscoverySetting(t *testing.T) {
	for settings, want := range map[string]bool{
		``: false, `{}`: false, `not json`: false,
		`{"quick_create_discovery":"off"}`:   false,
		`{"quick_create_discovery":"bogus"}`: false,
		`{"quick_create_discovery":"on"}`:    true,
		`{"quick_create_discovery":" On "}`:  true,
	} {
		if got := QuickCreateDiscovery([]byte(settings)); got != want {
			t.Errorf("QuickCreateDiscovery(%q) = %v, want %v", settings, got, want)
		}
	}
}

func TestDiscoveryFallback(t *testing.T) {
	with := &Definition{Steps: []Step{{StatusKey: "todo"}, {StatusKey: "investigating"}}}
	without := &Definition{Steps: []Step{{StatusKey: "backlog"}, {StatusKey: "todo"}}}
	custom := &Definition{Steps: []Step{{StatusKey: "backlog"}, {StatusKey: "todo"}}, StartStatusKey: "backlog"}
	for _, tc := range []struct {
		name   string
		def    *Definition
		status string
		want   string
	}{
		{"step listed keeps investigating", with, "investigating", "investigating"},
		{"no step falls back to todo", without, "investigating", "todo"},
		{"no step falls back to the start status", custom, "investigating", "backlog"},
		{"nil workflow falls back to todo", nil, "investigating", "todo"},
		{"other statuses pass through", without, "backlog", "backlog"},
		{"in_progress passes through", with, "in_progress", "in_progress"},
	} {
		if got := DiscoveryFallback(tc.def, tc.status); got != tc.want {
			t.Errorf("%s: DiscoveryFallback = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDiscoveryNote(t *testing.T) {
	long := strings.Repeat("x", 300)
	for _, tc := range []struct{ name, reason, want string }{
		{"plain", "no checkable done-when", "Discovery: no checkable done-when. Reply to veto."},
		{"one line", "  needs\na design\tlock. ", "Discovery: needs a design lock. Reply to veto."},
		{"empty gets the default", " ", "Discovery: the request is not clear enough to start. Reply to veto."},
		{"capped", long, "Discovery: " + long[:200] + ". Reply to veto."},
	} {
		if got := DiscoveryNote(tc.reason); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
