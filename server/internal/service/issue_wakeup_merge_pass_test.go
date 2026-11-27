package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/reviewindependence"
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

// say posts a comment on the wakeup's issue as the given author. An agent's
// comment carries the task that wrote it, as a comment posted through an agent
// run does (CHE-10267).
func (m *mergeFix) say(t *testing.T, authorType, authorID, content string, at time.Time) string {
	t.Helper()
	var task any
	if authorType == "agent" {
		task = m.f.Task(t, authorID, testutil.Cols{"issue_id": util.UUIDToString(m.issue), "status": "completed", "completed_at": testutil.Raw("now()")})
	}
	return m.sayFrom(t, authorType, authorID, content, at, task)
}

// sayFrom posts a comment with an explicit source_task_id (nil for none).
func (m *mergeFix) sayFrom(t *testing.T, authorType, authorID, content string, at time.Time, sourceTask any) string {
	t.Helper()
	return m.f.Insert(t, "comment", testutil.Cols{
		"workspace_id": m.f.WorkspaceID, "issue_id": util.UUIDToString(m.issue),
		"author_type": authorType, "author_id": authorID, "content": content, "type": "comment", "created_at": at, "updated_at": at,
		"source_task_id": sourceTask,
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
		"PASS comment with no source task": func(t *testing.T, m *mergeFix) {
			m.sayFrom(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), verdictLine("PASS", "a"), m.clock.Add(-time.Hour), nil)
		},
		"PASS comment whose task belongs to another agent": func(t *testing.T, m *mergeFix) {
			other := m.f.Task(t, m.author, testutil.Cols{"issue_id": util.UUIDToString(m.issue), "status": "completed", "completed_at": testutil.Raw("now()")})
			m.sayFrom(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), verdictLine("PASS", "a"), m.clock.Add(-time.Hour), other)
		},
		"PASS comment edited after it was written": func(t *testing.T, m *mergeFix) {
			id := m.say(t, "agent", m.signer(t, "terra", "gpt-6.1-terra"), verdictLine("PASS", "a"), m.clock.Add(-2*time.Hour))
			m.edit(t, id, verdictLine("PASS", "a")+"\nTypo fixed.")
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

// CHE-9319: with the review_independence setting present the PASS rule judges
// agents by the setting's values (keyed by runtime provider/model), not LabOf.
func (m *mergeFix) setProvider(t *testing.T, agentID, provider string) {
	t.Helper()
	m.f.Exec(t, "UPDATE agent_runtime SET provider=$2 WHERE id=(SELECT runtime_id FROM agent WHERE id=$1)", agentID, provider)
}

func (m *mergeFix) reviewIndependence(t *testing.T, agentValues string) {
	t.Helper()
	m.f.Exec(t, `UPDATE workspace SET settings=settings || jsonb_build_object('review_independence',
		jsonb_build_object('attribute','team','rule','reviewer_value_not_in_author_values','agent_values',$2::jsonb)) WHERE id=$1`, m.f.WorkspaceID, agentValues)
}

func TestDerivedPassUsesTheConfiguredValues(t *testing.T) {
	for name, tc := range map[string]struct {
		values   string
		wantRuns int
	}{
		"signer of another value":             {`{"prov-a/*":"x","prov-b/*":"y"}`, 1},
		"signer of the author's value":        {`{"prov-a/*":"x","prov-b/*":"x"}`, 0},
		"signer the setting does not list":    {`{"prov-a/*":"x"}`, 0},
		"author the setting does not list":    {`{"prov-b/*":"y"}`, 0},
		"exact model beats provider wildcard": {`{"prov-a/*":"x","prov-b/*":"x","prov-b/gpt-6.1-terra":"y"}`, 1},
	} {
		t.Run(name, func(t *testing.T) {
			m := passFixture(t)
			terra := m.signer(t, "terra", "gpt-6.1-terra")
			m.setProvider(t, m.author, "prov-a")
			m.setProvider(t, terra, "prov-b")
			m.reviewIndependence(t, tc.values)
			m.say(t, "agent", terra, verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
			for i := 0; i < 3; i++ {
				m.tick(t)
			}
			if n := m.runs(t); n != tc.wantRuns {
				t.Fatalf("%d runs, want %d", n, tc.wantRuns)
			}
		})
	}
}

// CHE-9319: the gate judges a signer against every value the PR's commits name,
// as routing does. The commits come from the PR snapshot the refresher stored;
// the gate calls no network.
func (m *mergeFix) storeCommits(t *testing.T, head string, commits ...reviewindependence.PRCommit) {
	t.Helper()
	raw, err := json.Marshal(reviewindependence.NewStoredCommits(head, commits))
	if err != nil {
		t.Fatal(err)
	}
	m.f.Exec(t, "UPDATE github_pull_request SET commit_identities=$2::jsonb WHERE id=$1", m.pr, string(raw))
}

func (m *mergeFix) commitSetting(t *testing.T) {
	t.Helper()
	m.f.Exec(t, `UPDATE workspace SET settings=settings || jsonb_build_object('review_independence',
		jsonb_build_object('attribute','team','rule','reviewer_value_not_in_author_values',
			'agent_values','{"prov-a/*":"x","prov-b/*":"y","prov-c/*":"z"}'::jsonb,
			'commit_identities','{"vendor-x.example":"x","vendor-y.example":"y"}'::jsonb)) WHERE id=$1`, m.f.WorkspaceID)
}

func TestDerivedPassJudgesTheSignerAgainstEveryCommitValue(t *testing.T) {
	byX := reviewindependence.PRCommit{SHA: "1111111111111111", AuthorEmail: "a@vendor-x.example", CommitterEmail: "a@vendor-x.example"}
	byY := reviewindependence.PRCommit{SHA: "2222222222222222", AuthorEmail: "b@vendor-y.example", CommitterEmail: "b@vendor-y.example"}
	unmapped := reviewindependence.PRCommit{SHA: "3333333333333333", AuthorEmail: "c@elsewhere.example", CommitterEmail: "c@elsewhere.example"}
	for name, tc := range map[string]struct {
		head     string
		commits  []reviewindependence.PRCommit
		signerOn string
		wantRuns int
	}{
		// The PR's author agent is valued x; a commit by y makes y an author too.
		"signer of a second commit value is refused":     {"a", []reviewindependence.PRCommit{byX, byY}, "prov-b", 0},
		"signer of the first commit value is refused":    {"a", []reviewindependence.PRCommit{byX, byY}, "prov-a", 0},
		"signer of a third value passes":                 {"a", []reviewindependence.PRCommit{byX, byY}, "prov-c", 1},
		"an unmapped commit keeps the run rule, y free":  {"a", []reviewindependence.PRCommit{byX, unmapped}, "prov-b", 1},
		"an unmapped commit keeps the run rule, x held":  {"a", []reviewindependence.PRCommit{byX, unmapped}, "prov-a", 0},
		"commits stored for another head are not used":   {"old", []reviewindependence.PRCommit{byX, byY}, "prov-b", 1},
		"commits never stored keep the run rule, y free": {"", nil, "prov-b", 1},
	} {
		t.Run(name, func(t *testing.T) {
			m := passFixture(t)
			terra := m.signer(t, "terra", "gpt-6.1-terra")
			m.setProvider(t, m.author, "prov-a")
			m.setProvider(t, terra, tc.signerOn)
			m.commitSetting(t)
			if tc.head != "" {
				m.storeCommits(t, tc.head, tc.commits...)
			}
			m.say(t, "agent", terra, verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
			for i := 0; i < 3; i++ {
				m.tick(t)
			}
			if n := m.runs(t); n != tc.wantRuns {
				t.Fatalf("%d runs, want %d", n, tc.wantRuns)
			}
		})
	}
}

// linkSiblingPR links a second PR to the wakeup's issue (a stacked PR) with the
// commits stored for storedHead, while its current head is head.
func (m *mergeFix) linkSiblingPR(t *testing.T, head, storedHead string, commits ...reviewindependence.PRCommit) {
	t.Helper()
	pr := m.f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": m.f.WorkspaceID, "installation_id": 1,
		"repo_owner": "fixture", "repo_name": "merge", "pr_number": 10,
		"title": "PR B", "state": "open", "html_url": "https://example.test/pr/10",
		"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
		"head_sha": head, "snapshot_head_sha": head, "snapshot_base_ref": "main",
	})
	m.f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
	m.f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) SELECT issue_id,$2 FROM issue_pull_request WHERE pull_request_id=$1", m.pr, pr)
	if storedHead != "" {
		raw, err := json.Marshal(reviewindependence.NewStoredCommits(storedHead, commits))
		if err != nil {
			t.Fatal(err)
		}
		m.f.Exec(t, "UPDATE github_pull_request SET commit_identities=$2::jsonb WHERE id=$1", pr, string(raw))
	}
}

// CHE-9319: the gate's author set covers every linked PR, as routing's does. A
// linked PR whose commits are missing or stale for its head keeps the run rule.
func TestDerivedPassJudgesTheSignerAgainstEveryLinkedPR(t *testing.T) {
	byX := reviewindependence.PRCommit{SHA: "1111111111111111", AuthorEmail: "a@vendor-x.example", CommitterEmail: "a@vendor-x.example"}
	byY := reviewindependence.PRCommit{SHA: "2222222222222222", AuthorEmail: "b@vendor-y.example", CommitterEmail: "b@vendor-y.example"}
	for name, tc := range map[string]struct {
		siblingHead, storedHead string
		signerOn                string
		wantRuns                int
	}{
		"signer of a sibling PR's value is refused":       {"s1", "s1", "prov-b", 0},
		"signer of a third value passes":                  {"s1", "s1", "prov-c", 1},
		"sibling commits stale for its head keep runs":    {"s2", "s1", "prov-b", 1},
		"sibling commits never stored keep the run rule":  {"s1", "", "prov-b", 1},
		"stale sibling still holds the PR author's value": {"s2", "s1", "prov-a", 0},
	} {
		t.Run(name, func(t *testing.T) {
			m := passFixture(t)
			terra := m.signer(t, "terra", "gpt-6.1-terra")
			m.setProvider(t, m.author, "prov-a")
			m.setProvider(t, terra, tc.signerOn)
			m.commitSetting(t)
			m.storeCommits(t, "a", byX)
			m.linkSiblingPR(t, tc.siblingHead, tc.storedHead, byY)
			m.say(t, "agent", terra, verdictLine("PASS", "a"), m.clock.Add(-time.Hour))
			for i := 0; i < 3; i++ {
				m.tick(t)
			}
			if n := m.runs(t); n != tc.wantRuns {
				t.Fatalf("%d runs, want %d", n, tc.wantRuns)
			}
		})
	}
}
