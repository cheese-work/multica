package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-5594: the cross-lab PASS and its time come from signed verdict comments
// on the wakeup's issue and the labs in the agent records, never from a fact a
// producer writes. Every test here builds the service the way main does.

// signer makes an agent whose record says it runs the given model.
func (m *mergeFix) signer(t *testing.T, label, model string) string {
	t.Helper()
	id := m.f.privateAgentOwnedBy(t, m.f.UserID, label)
	m.f.Exec(t, "UPDATE agent SET model=$2 WHERE id=$1", id, model)
	return id
}

// say posts a comment on the wakeup's issue as the given author.
func (m *mergeFix) say(t *testing.T, authorType, authorID, content string, at time.Time) string {
	t.Helper()
	return m.f.Insert(t, "comment", testutil.Cols{
		"workspace_id": m.f.WorkspaceID, "issue_id": util.UUIDToString(m.issue),
		"author_type": authorType, "author_id": authorID, "content": content, "type": "comment", "created_at": at, "updated_at": at,
	})
}

func verdictLine(word, head string) string {
	return fmt.Sprintf("**Verdict: %s** — PR #9, head `%s`.\n\nReviewer: signer.\nSigned: %s on this head.", word, head, word)
}

// passFixture stores a receipt and an author on Anthropic, with no PASS yet.
func passFixture(t *testing.T) *mergeFix {
	m := newMergeFix(t, nil, "")
	mainStyle(t, m)
	m.storedNoPass(t, "")
	return m
}

func TestDerivedPassDispatchesForACrossLabSignerOnTheExactHead(t *testing.T) {
	m := passFixture(t)
	terra := m.signer(t, "terra", "gpt-6.1-terra")
	m.say(t, "agent", terra, verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
	for i := 0; i < 3; i++ {
		m.tick(t)
	}
	if n := m.runs(t); n != 1 {
		t.Fatalf("%d runs for a PASS from another lab on the exact head, want 1", n)
	}
}

func TestDerivedPassHoldsWithoutAValidSignedPass(t *testing.T) {
	cases := map[string]func(*testing.T, *mergeFix){
		"no PASS comment at all": func(t *testing.T, m *mergeFix) {},
		"same-lab signer": func(t *testing.T, m *mergeFix) {
			m.say(t, "agent", m.signer(t, "opus", "claude-opus-5-5[1m]"), verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
		},
		"PASS on an older head": func(t *testing.T, m *mergeFix) {
			m.say(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), verdictLine("PASS", "z"), m.clock.Add(-time.Hour))
		},
		"verdict relayed by another agent": func(t *testing.T, m *mergeFix) {
			relay := verdictLine("PASS", "a") + "\nsigned by x99-codex-terra in 01a1d81f"
			m.say(t, "agent", m.signer(t, "gstack", "gpt-6.1-sol"), "**Verdict: PASS (round 2)**, signed by x99-codex-terra in 01a1d81f. PR #9, head `a`.\n"+relay, m.clock.Add(-time.Hour))
		},
		"PASS withdrawn by a later verdict from the same signer": func(t *testing.T, m *mergeFix) {
			terra := m.signer(t, "terra", "gpt-6.1-terra")
			m.say(t, "agent", terra, verdictLine("PASS", "a"), m.clock.Add(-2*time.Hour))
			m.say(t, "agent", terra, verdictLine("CHANGES REQUESTED", "a"), m.clock.Add(-time.Hour))
		},
		"PASS comment written by a member": func(t *testing.T, m *mergeFix) {
			m.say(t, "member", m.f.UserID, verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
		},
		"PASS comment deleted": func(t *testing.T, m *mergeFix) {
			id := m.say(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
			m.f.Exec(t, "UPDATE comment SET deleted_at=now() WHERE id=$1", id)
		},
		"PASS quoted below the first line": func(t *testing.T, m *mergeFix) {
			m.say(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), "Notes.\n"+verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
		},
		"signer whose lab is unknown": func(t *testing.T, m *mergeFix) {
			m.say(t, "agent", m.signer(t, "mystery", "some-local-model"), verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			m := newMergeFix(t, nil, "")
			mainStyle(t, m)
			m.storedNoPass(t, "")
			setup(t, m)
			for i := 0; i < 3; i++ {
				m.tick(t)
			}
			if n := m.runs(t); n != 0 {
				t.Fatalf("%d runs, want 0", n)
			}
		})
	}
}

func TestDerivedPassAuthorLabMustBeKnownAndDifferent(t *testing.T) {
	m := passFixture(t)
	m.f.Exec(t, "UPDATE agent SET model='gpt-6.1-sol' WHERE id=$1", m.author) // author now OpenAI too
	m.say(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
	m.tick(t)
	if n := m.runs(t); n != 0 {
		t.Fatalf("%d runs when author and signer share a lab, want 0", n)
	}
	m.f.Exec(t, "UPDATE agent SET model='some-local-model' WHERE id=$1", m.author)
	m.tick(t)
	if n := m.runs(t); n != 0 {
		t.Fatalf("%d runs when the author lab is unknown, want 0", n)
	}
}

func TestDerivedPassSignerCannotBeTheAuthor(t *testing.T) {
	m := passFixture(t)
	m.f.Exec(t, "UPDATE agent SET model='gpt-6.1-sol' WHERE id=$1", m.author)
	m.say(t, "agent", m.author, verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
	m.tick(t)
	if n := m.runs(t); n != 0 {
		t.Fatalf("%d runs for a PASS signed by the author, want 0", n)
	}
}

// The veto window of a gate-path PR counts from the PASS comment, not from a
// fact a producer writes.
func TestDerivedPassTimeStartsTheGateVetoWindow(t *testing.T) {
	m := passFixture(t)
	m.f.Exec(t, "UPDATE merge_receipt_fact SET paths=ARRAY['.github/workflows/ci.yml'] WHERE workspace_id=$1", m.f.WorkspaceID)
	m.say(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
	m.tick(t)
	if n := m.runs(t); n != 0 {
		t.Fatalf("%d runs one hour after the PASS on a gate path, want 0", n)
	}
	m.refresh(t, 25*time.Hour) // 26 hours after the PASS
	m.record(t, "receipt-1")
	m.f.Exec(t, "UPDATE merge_receipt_fact SET paths=ARRAY['.github/workflows/ci.yml'] WHERE workspace_id=$1", m.f.WorkspaceID)
	m.tick(t)
	m.tick(t)
	if n := m.runs(t); n != 1 {
		t.Fatalf("%d runs 26 hours after the PASS on a gate path, want 1", n)
	}
}

func TestDerivedPassCountsOnlyTheWakeupIssue(t *testing.T) {
	m := passFixture(t)
	other := m.f.Insert(t, "issue", testutil.Cols{"workspace_id": m.f.WorkspaceID, "title": "other", "status": "todo", "priority": "none", "creator_type": "member", "creator_id": m.f.UserID, "number": 90001})
	terra := m.signer(t, "terra", "gpt-6.1-terra")
	m.f.Insert(t, "comment", testutil.Cols{"workspace_id": m.f.WorkspaceID, "issue_id": other, "author_type": "agent", "author_id": terra,
		"content": verdictLine("PASS", "a"), "type": "comment", "created_at": m.clock.Add(-time.Hour), "updated_at": m.clock.Add(-time.Hour)})
	m.tick(t)
	if n := m.runs(t); n != 0 {
		t.Fatalf("%d runs for a PASS on an unrelated issue, want 0", n)
	}
}

// An edit changes what a verdict says now, so the edit time, not the creation
// time, orders it against the signer's other verdicts and starts the veto
// window. Edits go through a plain UPDATE of the content, as the comment
// update path does.
func (m *mergeFix) edit(t *testing.T, id, content string) {
	t.Helper()
	m.f.Exec(t, "UPDATE comment SET content=$2, updated_at=now() WHERE id=$1", id, content)
}

func TestDerivedPassEditedOlderVerdictWithdrawsANewerCreatedPass(t *testing.T) {
	m := passFixture(t)
	terra := m.signer(t, "terra", "gpt-6.1-terra")
	old := m.say(t, "agent", terra, verdictLine("PASS", "a"), m.clock.Add(-3*time.Hour))
	m.say(t, "agent", terra, verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
	m.tick(t)
	if n := m.live(t); n != 1 {
		t.Fatalf("%d live runs before the edit, want 1", n)
	}
	task := m.lastTask(t)
	m.edit(t, old, verdictLine("CHANGES", "a"))
	if err := m.s.CheckClaim(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("CheckClaim after an older verdict was edited to CHANGES = %v, want ErrWakeupForbidden", err)
	}
	m.tick(t)
	m.finishRuns(t)
	m.tick(t)
	if n := m.runs(t); n != 1 {
		t.Fatalf("%d runs in total after the edit, want the 1 from before", n)
	}
}

func TestDerivedPassEditedIntoPassCannotBackdateTheVetoWindow(t *testing.T) {
	m := passFixture(t)
	m.f.Exec(t, "UPDATE merge_receipt_fact SET paths=ARRAY['.github/workflows/ci.yml'] WHERE workspace_id=$1", m.f.WorkspaceID)
	id := m.say(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), verdictLine("CHANGES", "a"), m.clock.Add(-30*time.Hour))
	m.edit(t, id, verdictLine("PASS", "a"))
	m.tick(t)
	m.tick(t)
	if n := m.runs(t); n != 0 {
		t.Fatalf("%d runs for a gate path whose verdict became PASS by an edit just now, want 0", n)
	}
}
