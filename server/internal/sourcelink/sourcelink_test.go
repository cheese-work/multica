package sourcelink

import "testing"

const plan = "https://github.com/cheese-work/pocket-actual/blob/bc40d61ee615903f5511b69ddfa7f57c2020171e/.planning/phases/01-trusted-present-day-recovery/01-22-PLAN.md"

// The disposable-path case from the CHE-408 acceptance criteria: prose naming a
// source without linking it fails; the linked equivalent passes.
func TestCheckRejectsDisposableReferenceAndAcceptsLink(t *testing.T) {
	if v := Check("Read the authoritative 01-22 plan", plan); v.OK {
		t.Error("prose naming a source without a link must not satisfy the requirement")
	}
	if v := Check("Read the [authoritative 01-22 plan]("+plan+")", plan); !v.OK {
		t.Errorf("a real link to the declared source must pass, got %q", v.Reason)
	}
}

// A reachable-but-different URL is the criteria's explicit non-substitute.
func TestCheckRejectsDifferentURL(t *testing.T) {
	v := Check("see [the plan](https://github.com/cheese-work/pocket-actual/blob/main/README.md)", plan)
	if v.OK {
		t.Error("a different URL must not satisfy a declared source")
	}
}

// Revision matters: the declared source pins an immutable commit SHA, so the
// same file at a branch ref is a different, mutable destination.
func TestCheckRejectsSameFileAtDifferentRevision(t *testing.T) {
	mutable := "https://github.com/cheese-work/pocket-actual/blob/main/.planning/phases/01-trusted-present-day-recovery/01-22-PLAN.md"
	if v := Check("see [plan]("+mutable+")", plan); v.OK {
		t.Error("the declared revision must be honored; a branch ref is not the pinned commit")
	}
}

func TestCheckAcceptsLinkAmongOthers(t *testing.T) {
	body := "context in [issue](https://example.com/i/1), source is [plan](" + plan + "), also [other](https://other.example/o)"
	if v := Check(body, plan); !v.OK {
		t.Errorf("the declared source among other links must pass, got %q", v.Reason)
	}
}

// Code spans and fences are how an agent DISCUSSES a URL. Discussion is not
// citation, so they must not satisfy the requirement.
func TestCheckRejectsLinkOnlyInCodeConstructs(t *testing.T) {
	cases := []struct{ name, body string }{
		{"code span", "the source is `[plan](" + plan + ")`"},
		{"fenced block", "```\n[plan](" + plan + ")\n```"},
		{"bare url in code span", "the source is `" + plan + "`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v := Check(tc.body, plan); v.OK {
				t.Error("a destination only inside a code construct must not satisfy the requirement")
			}
		})
	}
}

// Quoting someone else's link is not declaring your own source.
func TestCheckRejectsQuotedOnlyLink(t *testing.T) {
	if v := Check("> they said see [plan]("+plan+")", plan); v.OK {
		t.Error("a link only inside a blockquote must not satisfy the requirement")
	}
	if v := Check("> they said see [plan]("+plan+")\n\nconfirmed: [plan]("+plan+")", plan); !v.OK {
		t.Error("a quoted link plus the author's own link must pass")
	}
}

// An autolink is a real clickable citation.
func TestCheckAcceptsAutolink(t *testing.T) {
	if v := Check("source: <"+plan+">", plan); !v.OK {
		t.Errorf("an autolink to the declared source must pass, got %q", v.Reason)
	}
}

// Placeholder and empty destinations are named rejects in the criteria.
func TestCheckRejectsPlaceholderDestinations(t *testing.T) {
	cases := []struct{ name, body string }{
		{"empty", "see [plan]()"},
		{"hash", "see [plan](#)"},
		{"todo", "see [plan](TODO)"},
		{"whitespace", "see [plan]( )"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v := Check(tc.body, plan); v.OK {
				t.Errorf("placeholder destination in %q must not satisfy the requirement", tc.body)
			}
		})
	}
}

// Runtime-local destinations are dead for every reader (MUL-4899 overlap).
func TestCheckRejectsRuntimeLocalDestination(t *testing.T) {
	if v := Check("see [plan](file:///home/agent/workdir/01-22-PLAN.md)", plan); v.OK {
		t.Error("a file:// destination must not satisfy the requirement")
	}
}

// Normalization: these differ only in ways that do not change what is fetched.
func TestCheckAcceptsEquivalentURLForms(t *testing.T) {
	cases := []struct{ name, linked string }{
		{"uppercase scheme", "HTTPS://github.com/cheese-work/pocket-actual/blob/bc40d61ee615903f5511b69ddfa7f57c2020171e/.planning/phases/01-trusted-present-day-recovery/01-22-PLAN.md"},
		{"uppercase host", "https://GITHUB.COM/cheese-work/pocket-actual/blob/bc40d61ee615903f5511b69ddfa7f57c2020171e/.planning/phases/01-trusted-present-day-recovery/01-22-PLAN.md"},
		{"fragment appended", plan + "#L10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v := Check("see [plan]("+tc.linked+")", plan); !v.OK {
				t.Errorf("equivalent form %q must satisfy the requirement, got %q", tc.linked, v.Reason)
			}
		})
	}
}

// Path case is NOT equivalent: it selects a different file on most servers.
func TestCheckRejectsDifferentPathCase(t *testing.T) {
	shouty := "https://github.com/cheese-work/pocket-actual/blob/bc40d61ee615903f5511b69ddfa7f57c2020171e/.planning/phases/01-trusted-present-day-recovery/01-22-plan.MD"
	if v := Check("see [plan]("+shouty+")", plan); v.OK {
		t.Error("path case must be significant; it can select a different file")
	}
}

// No declared source means nothing to enforce.
func TestCheckPassesWhenNoSourceDeclared(t *testing.T) {
	if v := Check("Read the authoritative 01-22 plan", ""); !v.OK {
		t.Error("an undeclared source must not constrain the body")
	}
}

// The diagnostic must name the required source and the failure, so the agent
// can fix it without guessing (acceptance criteria: actionable diagnostics).
func TestCheckReasonNamesRequiredSource(t *testing.T) {
	v := Check("Read the authoritative 01-22 plan", plan)
	if v.OK {
		t.Fatal("expected failure")
	}
	if v.Reason == "" {
		t.Fatal("a failure must carry a reason")
	}
	if v.RequiredSource != plan {
		t.Errorf("RequiredSource = %q, want the declared source", v.RequiredSource)
	}
}

// An already-linked body passes unchanged: the guard never rewrites content.
func TestCheckDoesNotRequireDuplicateLinks(t *testing.T) {
	body := "single citation: [plan](" + plan + ")"
	if v := Check(body, plan); !v.OK {
		t.Error("one link must be enough; a duplicate must never be required")
	}
}
