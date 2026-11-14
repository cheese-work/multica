package issueworkflow

import (
	"strings"
	"testing"
)

// upstream_checked (CHE-5583, CHE-5574 step 6): a create-time gate. The
// value and title rules are pure; the handler tests drive the real create.

const projectA = "5e621e12-eeb4-40a5-9571-25657d5b0ec5"

func TestUpstreamCheckValue(t *testing.T) {
	cases := []struct {
		value string
		valid bool
	}{
		{"https://github.com/multica-ai/multica/pull/1", true},
		{"https://github.com/multica-ai/multica/releases/tag/v0.9.0", true},
		{"none found: multica notification digest", true},
		{"none found: abc", true},
		{"not multica: dotfiles-agents", true},
		{"not multica: doc", true},
		{"", false},
		{"none found", false},
		{"none found:", false},
		{"none found: ab", false},
		{"none found:    ", false},
		{"not multica:", false},
		{"not multica: x", false},
		{"https://github.com/other-org/multica/pull/1", false},
		{"https://github.com/multica-ai/multica-evil/pull/1", false},
		{"http://github.com/multica-ai/multica/pull/1", false},
		{"https://github.com/multica-ai/multica", false},
		{"whatever", false},
	}
	for _, c := range cases {
		if got := UpstreamCheckValid(c.value); got != c.valid {
			t.Errorf("UpstreamCheckValid(%q) = %v, want %v", c.value, got, c.valid)
		}
	}
}

func TestUpstreamGatedTitle(t *testing.T) {
	cases := []struct {
		title          string
		gated, untyped bool
	}{
		{"feat(x): a thing", true, false},
		{"Feat: a thing", true, false},
		{"plan(x)!: a plan", true, false},
		{"chore(x): a thing", false, false},
		{"fix: a thing", false, false},
		{"design(x): a thing", false, false},
		{"no prefix at all", true, true},
	}
	for _, c := range cases {
		gated, untyped := UpstreamGatedTitle(c.title)
		if gated != c.gated || untyped != c.untyped {
			t.Errorf("UpstreamGatedTitle(%q) = %v,%v want %v,%v", c.title, gated, untyped, c.gated, c.untyped)
		}
	}
}

func TestUpstreamCheckedIsAnEntryGateWithProjects(t *testing.T) {
	ok := Step{StatusKey: "backlog", Entry: []EntryGate{{Guard: "upstream_checked", Mode: EntryModeBlock, Projects: []string{projectA}}}}
	if err := validateEntry(ok); err != nil {
		t.Fatalf("valid gate: %v", err)
	}
	for name, projects := range map[string][]string{"empty": nil, "not a uuid": {"multica"}, "one bad": {projectA, "x"}} {
		s := Step{StatusKey: "backlog", Entry: []EntryGate{{Guard: "upstream_checked", Mode: EntryModeBlock, Projects: projects}}}
		err := validateEntry(s)
		if err == nil || !strings.Contains(err.Error(), "projects") {
			t.Errorf("%s: err = %v, want a projects error", name, err)
		}
	}
	other := Step{StatusKey: "backlog", Entry: []EntryGate{{Guard: "pr.ready", Mode: EntryModeBlock, Projects: []string{projectA}}}}
	if err := validateEntry(other); err == nil {
		t.Error("projects on another guard must be rejected")
	}
}

func TestUpstreamCheckedKeepsProjectsAndNeverBlocksAStatusWrite(t *testing.T) {
	norm := normalizeEntry([]EntryGate{{Guard: " upstream_checked ", Mode: " block ", Projects: []string{" " + projectA + " "}}})
	if len(norm) != 1 || len(norm[0].Projects) != 1 || norm[0].Projects[0] != projectA {
		t.Fatalf("normalizeEntry lost or kept untrimmed projects: %+v", norm)
	}
	if got := EvaluateEntry(norm, Facts{}); len(got) != 0 {
		t.Fatalf("a status write into the step was refused by a create-time gate: %+v", got)
	}
}

func TestUpstreamCheckedIsNotAnEdgeGuard(t *testing.T) {
	def := Definition{Steps: []Step{{StatusKey: "backlog"}, {StatusKey: "todo"}}}
	err := validateEdges(Step{StatusKey: "backlog", Edges: []Edge{{To: "todo", Guard: "upstream_checked"}}}, def)
	if err == nil || !strings.Contains(err.Error(), "entry gate") {
		t.Fatalf("err = %v, want an entry-gate-only error", err)
	}
}

func TestUpstreamGateModeFor(t *testing.T) {
	other := "11111111-1111-4111-8111-111111111111"
	def := &Definition{Steps: []Step{
		{StatusKey: "backlog", Entry: []EntryGate{{Guard: "upstream_checked", Mode: EntryModeWarn, Projects: []string{projectA}}}},
		{StatusKey: "todo", Entry: []EntryGate{{Guard: "upstream_checked", Mode: EntryModeBlock, Projects: []string{projectA}}, {Guard: "upstream_checked", Mode: EntryModeBlock, Projects: []string{other}}}},
		{StatusKey: "done", Entry: []EntryGate{{Guard: "upstream_checked", Mode: EntryModeOff, Projects: []string{other}}}},
	}}
	if got := UpstreamGateMode(def, projectA); got != EntryModeBlock {
		t.Errorf("project A mode = %q, want block", got)
	}
	if got := UpstreamGateMode(def, "22222222-2222-4222-8222-222222222222"); got != "" {
		t.Errorf("an unlisted project is gated: %q", got)
	}
	if got := UpstreamGateMode(def, ""); got != "" {
		t.Errorf("no project is gated: %q", got)
	}
}
