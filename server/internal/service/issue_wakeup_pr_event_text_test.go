package service

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Fallback wake text per PR event kind (CHE-2298 d6). The tests drive the real
// bridge: an event row goes in, a tick queues the wake, and the instruction is
// read back from the task's note.

// asKind turns the env's head_changed event into another kind.
func (e prEventEnv) asKind(t *testing.T, kind, previousHead string) {
	t.Helper()
	e.f.Exec(t, `UPDATE issue_pr_event SET kind=$2, previous_head_sha=$3, dedupe_key=$2||'|new-head' WHERE id=$1`, e.event, kind, previousHead)
}

func (e prEventEnv) failedCheckRun(t *testing.T, name string) {
	t.Helper()
	var prID string
	e.f.QueryRow(t, `SELECT pull_request_id FROM issue_pr_event WHERE id=$1`, e.event).Scan(&prID)
	e.f.Cleanup(t, `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, prID)
	e.f.Exec(t, `INSERT INTO github_pull_request_check_run (pr_id, head_sha, ordinal, name, status, conclusion) VALUES ($1, 'new-head', 0, $2, 'completed', 'failure')`, prID, name)
}

// instruction is the text between the note header and the trigger facts.
func (e prEventEnv) instruction(t *testing.T) string {
	t.Helper()
	note := e.attentionNote(t)
	_, rest, ok := strings.Cut(note, "Instruction:\n")
	text, _, ok2 := strings.Cut(rest, "\nTrigger facts")
	if !ok || !ok2 {
		t.Fatalf("note has no instruction block: %q", note)
	}
	return text
}

func (e prEventEnv) wake(t *testing.T) string {
	t.Helper()
	e.endWhileDelivering(t)
	e.tickAll(t, e.s)
	if got := e.tasks(t, SystemRulePRAttention); got != 1 {
		t.Fatalf("next turn started %d runs, want 1", got)
	}
	return e.instruction(t)
}

func requireNoBlankField(t *testing.T, text string) {
	t.Helper()
	for _, blank := range []string{"  ", " .", " ,", "is .", "was .", "base .", "head .", ": ."} {
		if strings.Contains(text, blank) {
			t.Fatalf("instruction has an empty field (%q): %q", blank, text)
		}
	}
}

func requireAbsent(t *testing.T, text string, banned ...string) {
	t.Helper()
	for _, b := range banned {
		if strings.Contains(strings.ToLower(text), strings.ToLower(b)) {
			t.Fatalf("instruction carries %q for a kind that is not a conflict: %q", b, text)
		}
	}
}

func TestHeadChangedFallbackWakeNamesBothHeadsAndNoConflict(t *testing.T) {
	e := newPREventEnv(t)
	e.asKind(t, "head_changed", "old-head")

	text := e.wake(t)

	for _, want := range []string{"#903", "new-head", "old-head"} {
		if !strings.Contains(text, want) {
			t.Fatalf("instruction lacks %q: %q", want, text)
		}
	}
	requireAbsent(t, text, "cannot merge", "resolve the conflicts", "merge state")
	requireNoBlankField(t, text)
}

// A head change with no earlier head on record still names the new head.
func TestHeadChangedFallbackWakeOmitsAnUnknownOldHead(t *testing.T) {
	e := newPREventEnv(t)
	e.asKind(t, "head_changed", "")

	text := e.wake(t)

	if !strings.Contains(text, "new-head") {
		t.Fatalf("instruction lacks the new head: %q", text)
	}
	requireAbsent(t, text, "cannot merge", "resolve the conflicts", "replacing")
	requireNoBlankField(t, text)
}

func TestConflictFallbackWakeHasNoEmptyField(t *testing.T) {
	e := newPREventEnv(t)
	e.asKind(t, "conflict", "")

	text := e.wake(t)

	for _, want := range []string{"#903", "conflict", "new-head", "base-1", "Do not merge"} {
		if !strings.Contains(text, want) {
			t.Fatalf("instruction lacks %q: %q", want, text)
		}
	}
	requireNoBlankField(t, text)
}

func TestChecksFailedFallbackWakeNamesTheFailedCheck(t *testing.T) {
	e := newPREventEnv(t)
	e.asKind(t, "checks_failed", "")
	e.failedCheckRun(t, "unit-tests")

	text := e.wake(t)

	for _, want := range []string{"#903", "new-head", "unit-tests"} {
		if !strings.Contains(text, want) {
			t.Fatalf("instruction lacks %q: %q", want, text)
		}
	}
	requireAbsent(t, text, "cannot merge", "resolve the conflicts")
	requireNoBlankField(t, text)
}

// One wake that carries two causes names both.
func TestFallbackWakeWithTwoCausesNamesBoth(t *testing.T) {
	e := newPREventEnv(t)
	e.asKind(t, "head_changed", "old-head")
	e.f.Exec(t, `INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key)
		SELECT workspace_id, issue_id, pull_request_id, 'checks_failed', head_sha, base_sha, now() + interval '1 second', 'checks_failed|new-head' FROM issue_pr_event WHERE id=$1`, e.event)
	e.failedCheckRun(t, "lint")

	text := e.wake(t)

	for _, want := range []string{"old-head", "lint"} {
		if !strings.Contains(text, want) {
			t.Fatalf("instruction lacks %q: %q", want, text)
		}
	}
	requireAbsent(t, text, "cannot merge", "resolve the conflicts")
	requireNoBlankField(t, text)
}

// Each cause names its own PR, not the newest receipt's.
func TestFallbackWakeNamesEachCausesOwnPR(t *testing.T) {
	e := newPREventEnv(t)
	e.asKind(t, "head_changed", "old-head")
	var second string
	e.f.QueryRow(t, `INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha)
		VALUES ($1, 1, 'acme', 'widget', 904, 'another PR', 'open', 'https://example.test/pr904', now(), now(), 'new-head-904') RETURNING id`, e.f.WorkspaceID).Scan(&second)
	e.f.Cleanup(t, `DELETE FROM github_pull_request WHERE id=$1`, second)
	e.f.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id=$1`, second)
	e.f.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, e.issue, second)
	e.f.Exec(t, `INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, previous_head_sha, base_sha, observed_at, dedupe_key)
		VALUES ($1, $2, $3, 'head_changed', 'new-head-904', 'old-head-904', 'base-1', now() + interval '1 second', 'head_changed|new-head-904')`, e.f.WorkspaceID, e.issue, second)

	text := e.wake(t)

	for _, want := range []string{
		"Linked PR #903 has a new head: new-head, replacing old-head.",
		"Linked PR #904 has a new head: new-head-904, replacing old-head-904.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("instruction lacks %q: %q", want, text)
		}
	}
}

// A mergeability cause that joins a pr.event cause in one wake keeps its text.
func TestFallbackWakeKeepsACoalescedMergeabilityCause(t *testing.T) {
	e := newPREventEnv(t)
	e.asKind(t, "head_changed", "old-head")
	err := e.s.TriggerPullRequestWakeup(context.Background(), e.issue, PullRequestWakeupInput{Rule: SystemRulePRAttention,
		RepoOwner: "acme", RepoName: "widget", Number: 903, URL: "https://github.com/acme/widget/pull/903",
		HeadSHA: "new-head", BaseBranch: "main", Reason: PRAttentionMergeability, MergeState: "DIRTY"})
	if err != nil {
		t.Fatal(err)
	}

	text := e.wake(t)

	for _, want := range []string{"replacing old-head", "cannot merge as is", "merge state is DIRTY", "against base main", "Do not merge"} {
		if !strings.Contains(text, want) {
			t.Fatalf("instruction lacks %q: %q", want, text)
		}
	}
	requireNoBlankField(t, text)
}

// The renderer keeps a returned-to-draft cause next to a pr.event cause.
func TestAttentionInstructionKeepsAReturnedToDraftCause(t *testing.T) {
	receipts := []db.IssueWakeupReceipt{
		{EventType: wakeupPREventType, Payload: []byte(`{"pr_number":903,"kind":"head_changed","head_sha":"new-head","old_head_sha":"old-head"}`)},
		{EventType: "pr.returned_to_draft", Payload: []byte(`{"pr_number":903,"reason":"returned_to_draft","head_sha":"new-head","issue_status":"in_review"}`)},
	}
	text := systemWakeupInstruction(db.IssueWakeup{SystemRule: pgtype.Text{String: SystemRulePRAttention, Valid: true}}, nil, receipts)
	for _, want := range []string{"replacing old-head", "went back to draft at head new-head"} {
		if !strings.Contains(text, want) {
			t.Fatalf("instruction lacks %q: %q", want, text)
		}
	}
}
