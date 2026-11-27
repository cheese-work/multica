package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-10267: VerifiedSigners reads who signed a PASS on the exact head, from
// comments an agent run wrote and nobody edited. Every test builds the fixture
// of the merge PASS tests: an Anthropic PR author bound by the workspace.

func protectionOn() bool  { return true }
func protectionOff() bool { return false }

func (m *mergeFix) readErr(t *testing.T, protection CommentProtection) (VerifiedSignersResult, error) {
	t.Helper()
	tx, err := m.f.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return VerifiedSigners(context.Background(), tx, m.f.q, MergeCandidate{
		WorkspaceID: parseTestUUID(t, m.f.WorkspaceID), IssueID: m.issue, Owner: "fixture", Repo: "merge", Number: 9, HeadSHA: "a",
	}, protection)
}

func (m *mergeFix) read(t *testing.T, protection CommentProtection) VerifiedSignersResult {
	t.Helper()
	got, err := m.readErr(t, protection)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func verdictBy(word, head, reviewer string) string {
	return fmt.Sprintf("**Verdict: %s** — PR #9, head `%s`.\n\nReviewer: %s.", word, head, reviewer)
}

func (m *mergeFix) wantSigners(t *testing.T, want ...string) VerifiedSignersResult {
	t.Helper()
	got := m.read(t, protectionOn)
	if len(got.Signers) != len(want) {
		t.Fatalf("signers = %+v, want agents %v", got.Signers, want)
	}
	have := map[string]bool{}
	for _, s := range got.Signers {
		have[s.AgentID] = true
	}
	for _, id := range want {
		if !have[id] {
			t.Fatalf("signers = %+v, missing %s", got.Signers, id)
		}
	}
	return got
}

// One agent that writes two PASS comments under different Reviewer names is one
// signer (root done-when 1).
func TestVerifiedSignersOneAgentWithTwoReviewerNamesIsOneSigner(t *testing.T) {
	m := passFixture(t)
	terra := m.signer(t, "terra", "gpt-6.1-terra")
	m.say(t, "agent", terra, verdictBy("PASS", "a", "reviewer-one"), m.clock.Add(-2*time.Hour))
	m.say(t, "agent", terra, verdictBy("PASS", "a", "reviewer-two"), m.clock.Add(-time.Hour))
	got := m.wantSigners(t, terra)
	if got.Signers[0].Lab != "openai" || got.Signers[0].AgentName == "" || got.Signers[0].CommentID == "" {
		t.Fatalf("signer = %+v, want a named openai signer with its comment", got.Signers[0])
	}
}

// Two agents are two signers (root done-when 2); the newest PASS time is each
// signer's comment time.
func TestVerifiedSignersTwoAgentsAreTwoSigners(t *testing.T) {
	m := passFixture(t)
	terra, sol := m.signer(t, "terra", "gpt-6.1-terra"), m.signer(t, "sol", "gpt-6.1-sol")
	at := m.clock.Add(-time.Hour).UTC()
	m.say(t, "agent", terra, verdictBy("PASS", "a", "reviewer-one"), m.clock.Add(-2*time.Hour))
	id := m.say(t, "agent", sol, verdictBy("PASS", "a", "reviewer-two"), at)
	got := m.wantSigners(t, terra, sol)
	for _, s := range got.Signers {
		if s.AgentID == sol && (s.CommentID != id || !s.VerdictAt.Equal(at)) {
			t.Fatalf("sol signer = %+v, want comment %s at %s", s, id, at)
		}
	}
}

func TestVerifiedSignersEditedPassDoesNotCount(t *testing.T) {
	m := passFixture(t)
	id := m.say(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), verdictBy("PASS", "a", "reviewer-one"), m.clock.Add(-2*time.Hour))
	m.edit(t, id, verdictBy("PASS", "a", "reviewer-one")+"\nTypo fixed.")
	m.wantSigners(t)
}

func TestVerifiedSignersAFailEditedIntoPassDoesNotCount(t *testing.T) {
	m := passFixture(t)
	id := m.say(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), verdictBy("FAIL", "a", "reviewer-one"), m.clock.Add(-2*time.Hour))
	m.edit(t, id, verdictBy("PASS", "a", "reviewer-one"))
	m.wantSigners(t)
}

func TestVerifiedSignersNeedsTheTaskOfTheSameAgent(t *testing.T) {
	m := passFixture(t)
	terra := m.signer(t, "terra", "gpt-6.1-terra")
	m.sayFrom(t, "agent", terra, verdictBy("PASS", "a", "reviewer-one"), m.clock.Add(-time.Hour), nil)
	m.wantSigners(t)
	sol := m.signer(t, "sol", "gpt-6.1-sol")
	solTask := m.f.Task(t, sol, testutil.Cols{"issue_id": util.UUIDToString(m.issue), "status": "completed", "completed_at": testutil.Raw("now()")})
	m.sayFrom(t, "agent", terra, verdictBy("PASS", "a", "reviewer-one"), m.clock.Add(-time.Hour), solTask)
	m.wantSigners(t)
}

func TestVerifiedSignersIgnoresWhatIsNotAnAgentVerdictOnTheHead(t *testing.T) {
	m := passFixture(t)
	terra := m.signer(t, "terra", "gpt-6.1-terra")
	m.say(t, "agent", terra, "**Verdict: PASS**, signed by reviewer-two. PR #9, head `a`.", m.clock.Add(-time.Hour))
	m.say(t, "agent", terra, verdictBy("PASS", "z", "x"), m.clock.Add(-time.Hour))
	m.say(t, "agent", terra, "Notes.\n"+verdictBy("PASS", "a", "x"), m.clock.Add(-time.Hour))
	m.say(t, "member", m.f.UserID, verdictBy("PASS", "a", "x"), m.clock.Add(-time.Hour))
	gone := m.say(t, "agent", terra, verdictBy("PASS", "a", "x"), m.clock.Add(-time.Hour))
	m.f.Exec(t, "UPDATE comment SET deleted_at=now() WHERE id=$1", gone)
	m.wantSigners(t)
}

func TestVerifiedSignersANewerFailWithdrawsThePass(t *testing.T) {
	m := passFixture(t)
	terra, sol := m.signer(t, "terra", "gpt-6.1-terra"), m.signer(t, "sol", "gpt-6.1-sol")
	m.say(t, "agent", terra, verdictBy("PASS", "a", "x"), m.clock.Add(-3*time.Hour))
	m.say(t, "agent", terra, verdictBy("FAIL", "a", "x"), m.clock.Add(-2*time.Hour))
	m.say(t, "agent", sol, verdictBy("FAIL", "a", "x"), m.clock.Add(-3*time.Hour))
	m.say(t, "agent", sol, verdictBy("PASS", "a", "x"), m.clock.Add(-2*time.Hour))
	m.wantSigners(t, sol)
}

func TestVerifiedSignersAnEditedFailStillWithdraws(t *testing.T) {
	m := passFixture(t)
	terra := m.signer(t, "terra", "gpt-6.1-terra")
	m.say(t, "agent", terra, verdictBy("PASS", "a", "x"), m.clock.Add(-3*time.Hour))
	id := m.say(t, "agent", terra, verdictBy("PASS", "a", "x"), m.clock.Add(-2*time.Hour))
	m.edit(t, id, verdictBy("FAIL", "a", "x"))
	m.wantSigners(t)
}

func TestVerifiedSignersNeverCountsThePRAuthor(t *testing.T) {
	m := passFixture(t)
	m.say(t, "agent", m.author, verdictBy("PASS", "a", "x"), m.clock.Add(-time.Hour))
	got := m.read(t, protectionOn)
	if len(got.Signers) != 0 {
		t.Fatalf("signers = %+v, want none: the PR author cannot sign", got.Signers)
	}
	if got.PRAuthor == nil || got.PRAuthor.AgentID != m.author || got.PRAuthor.Lab != "anthropic" {
		t.Fatalf("pr_author = %+v, want the bound anthropic author %s", got.PRAuthor, m.author)
	}
}

func TestVerifiedSignersKeepsASignerWhoseLabIsUnknown(t *testing.T) {
	m := passFixture(t)
	mystery := m.signer(t, "mystery", "some-local-model")
	m.say(t, "agent", mystery, verdictBy("PASS", "a", "x"), m.clock.Add(-time.Hour))
	got := m.wantSigners(t, mystery)
	if got.Signers[0].Lab != "" {
		t.Fatalf("lab = %q, want empty for an unknown lab", got.Signers[0].Lab)
	}
}

func TestVerifiedSignersPRAuthorIsNilWhenUnknown(t *testing.T) {
	m := passFixture(t)
	m.f.Exec(t, "UPDATE workspace SET settings=settings - 'github_review_authors' WHERE id=$1", m.f.WorkspaceID)
	if got := m.read(t, protectionOn); got.PRAuthor != nil {
		t.Fatalf("pr_author = %+v, want nil when the PR author is unknown", got.PRAuthor)
	}
}

// A lookup fault must not read as an unknown author: with the author unknown the
// author's own PASS could count as a signer.
func TestVerifiedSignersAuthorLookupFaultReturnsNoSignerFacts(t *testing.T) {
	m := passFixture(t)
	m.f.Exec(t, `UPDATE workspace SET settings = jsonb_set(settings, '{github_review_authors}', '"unreadable"') WHERE id=$1`, m.f.WorkspaceID)
	got, err := m.readErr(t, protectionOn)
	if err == nil {
		t.Fatalf("err = nil, want the lookup error; got %+v", got)
	}
	if len(got.Signers) != 0 || got.PRAuthor != nil {
		t.Fatalf("result = %+v, want no facts on a lookup error", got)
	}
}

func TestVerifiedSignersReportsCommentProtection(t *testing.T) {
	m := passFixture(t)
	for name, c := range map[string]struct {
		probe CommentProtection
		want  bool
	}{"on": {protectionOn, true}, "off": {protectionOff, false}, "no probe": {nil, false}} {
		if got := m.read(t, c.probe).CommentProtection; got != c.want {
			t.Fatalf("%s: comment_protection = %v, want %v", name, got, c.want)
		}
	}
}
