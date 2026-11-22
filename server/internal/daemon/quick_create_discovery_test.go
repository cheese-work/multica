package daemon

import (
	"strings"
	"testing"
)

// CHE-9075 (C1, R12): with QuickCreateDiscovery the prompt tells the agent to
// classify the request (plan r4 A2) and file an unclear one in `investigating`.
// Without it the prompt is the historical one, byte for byte.

func TestQuickCreateDiscoveryOffIsByteIdentical(t *testing.T) {
	base := Task{QuickCreatePrompt: "fix the login button color"}
	for _, task := range []Task{
		base,
		{QuickCreatePrompt: base.QuickCreatePrompt, QuickCreateFill: true},
	} {
		want := buildQuickCreatePrompt(task)
		task.QuickCreateDiscovery = false
		if got := buildQuickCreatePrompt(task); got != want {
			t.Error("discovery=false changed the prompt")
		}
		if strings.Contains(want, "investigating") || strings.Contains(want, "Discovery") {
			t.Errorf("discovery=false prompt mentions Discovery:\n%s", want)
		}
	}
}

func TestQuickCreateDiscoveryOnClassifiesRequest(t *testing.T) {
	out := buildQuickCreatePrompt(Task{QuickCreatePrompt: "make onboarding better", QuickCreateDiscovery: true})
	for _, want := range []string{
		"clear",
		"checkable done-when",
		"open product-direction choice",
		"2 hours or less",
		"`--status investigating`",
		"Discovery: <reason>. Reply to veto.",
		"issue comment add",
		"only if it is `investigating`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("discovery=on prompt missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "- **status**: omit (defaults to `todo`).") {
		t.Error("discovery=on prompt keeps the unconditional status rule")
	}
	if !strings.Contains(out, "clear request: omit `--status`") {
		t.Error("discovery=on prompt must keep a clear request on the default status")
	}
}

func TestQuickCreateDiscoveryOnNeedsNoProjectLookup(t *testing.T) {
	// The server judges the workflow it picks, so the run never looks one up.
	for _, task := range []Task{
		{QuickCreatePrompt: "x", QuickCreateDiscovery: true, QuickCreateFill: true},
		{QuickCreatePrompt: "x", QuickCreateDiscovery: true, ProjectID: "11111111-1111-1111-1111-111111111111"},
	} {
		out := buildQuickCreatePrompt(task)
		for _, bad := range []string{"multica project get", "multica workflow get", "last line of the description"} {
			if strings.Contains(out, bad) {
				t.Errorf("prompt contains %q:\n%s", bad, out)
			}
		}
	}
}
