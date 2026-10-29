package service

import (
	"strings"
	"testing"
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
