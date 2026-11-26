package handler

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// When no card can open (a workspace with nobody to ask), the issue says so in
// a system comment: need_decision never lacks its card silently.
func TestRoundCap_MissingCardIsAnnouncedOnTheIssue(t *testing.T) {
	suffix := workflowTestSuffix()
	wsID := dbfx.Workspace(t, "Empty "+suffix, "empty-"+suffix)
	issueID := dbfx.Issue(t, "no one to ask", testutil.Cols{"workspace_id": wsID})
	issue, err := testHandler.Queries.GetIssue(t.Context(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.ensureReviewRoundsCard(t.Context(), issue, roundsCap{rounds: 3})
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'system' AND content LIKE '%could not open its decision card%'`, issueID); n != 1 {
		t.Fatalf("missing-card notices = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("decision requests = %d, want 0", n)
	}
}

// The card is all a decider reads. Its Split option must describe the
// same-issue Split rule: named work is removed, the issue and session go on,
// the round count, candidate and open findings carry over, and the removed work
// becomes one backlog issue. The old "split into smaller issues, each with a
// fresh round count" practice must be gone from every field of the card.
func TestRoundCap_CardSplitOptionDescribesSameIssueSplit(t *testing.T) {
	ask := reviewRoundsAsk(db.Issue{Title: "t"}, 3)
	var split *decisiontx.AskOption
	for i := range ask.Options {
		if ask.Options[i].Label == "Split the work" {
			split = &ask.Options[i]
		}
	}
	if split == nil {
		t.Fatal("card has no \"Split the work\" option")
	}
	option := strings.ToLower(split.WhatHappens + " " + strings.Join(split.Pros, " ") + " " + strings.Join(split.Cons, " ") + " " + split.CostOrTime)
	for _, want := range []string{"same issue", "session", "round count", "candidate", "open findings", "one backlog issue"} {
		if !strings.Contains(option, want) {
			t.Errorf("Split option does not mention %q: %s", want, option)
		}
	}
	card := strings.ToLower(ask.Decision + " " + ask.Recommendation + " " + option)
	for _, gone := range []string{"smaller issues", "its own review rounds", "fresh round count", "move to the issue they belong to"} {
		if strings.Contains(card, gone) {
			t.Errorf("card still carries the old Split rule text %q", gone)
		}
	}
}
