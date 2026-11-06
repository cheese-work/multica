package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

// deliverEpisodeOnly runs the episode stage alone. decisionPass would run the
// lifecycle stage first, which cancels the bound requests itself (design §9.10,
// traces A and B); trace C needs the episode to act first.
func (f episodeFixture) deliverEpisodeOnly(t *testing.T) {
	t.Helper()
	waitID := f.onlyEpisode(t).id
	if err := testHandler.deliverDecisionItem(context.Background(), parseUUID(testWorkspaceID), parseUUID(waitID), "episode"); err != nil {
		t.Fatal(err)
	}
}

func (f episodeFixture) closeIssue(t *testing.T, status string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, f.issueID, status)
	// The activity listener is not running in handler tests; record the status change it would write.
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details) VALUES ($1, $2, 'system', 'status_changed', jsonb_build_object('from', 'in_progress', 'to', $3::text))`, testWorkspaceID, f.issueID, status)
}

func (f episodeFixture) requestState(t *testing.T, requestID string) (status, reason string) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT status, COALESCE(cancel_reason, '') FROM decision_request WHERE id = $1`, requestID).Scan(&status, &reason)
	return status, reason
}

func (f episodeFixture) dmNotes(t *testing.T, requestID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM chat_message m JOIN decision_request r ON r.chat_session_id = m.chat_session_id
		WHERE r.id = $1 AND m.message_kind = 'decision_cancel_notice'`, requestID)
}

// Design §9.10 trace C: the issue closes while the episode is the first writer.
func TestDecisionEpisodeSystemCloseIssueClosed(t *testing.T) {
	f := newEpisodeFixture(t, false)
	second := f.secondIntentRequest(t)
	f.enter(t)
	answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
	f.closeIssue(t, "cancelled")

	f.deliverEpisodeOnly(t)

	closed := f.closedEpisodes(t)
	if len(closed) != 1 || closed[0].reason != "issue_closed" || closed[0].resumeTaskID != "" {
		t.Fatalf("closed episodes = %+v, want one closed issue_closed without a resume task", closed)
	}
	for _, requestID := range []string{f.requestID, second} {
		if status, reason := f.requestState(t, requestID); status != "cancelled" || reason != "issue_closed" {
			t.Fatalf("request %s = %s/%s, want cancelled/issue_closed", requestID, status, reason)
		}
	}
	if woken, outcome := f.answerState(t, answerID); !woken || outcome != "skipped_ineligible" {
		t.Fatalf("answer woken=%v outcome=%q, want skipped_ineligible", woken, outcome)
	}
	if got := f.continuations(t); got != 0 {
		t.Fatalf("continuation runs = %d, want none", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'cancel'`, f.issueID); got != 2 {
		t.Fatalf("cancel comments = %d, want one per request", got)
	}
	// An answered-only session writes no note; the open request's session writes one.
	if got := f.dmNotes(t, f.requestID); got != 0 {
		t.Fatalf("notes for the answered request = %d, want 0", got)
	}
	if got := f.dmNotes(t, second); got != 1 {
		t.Fatalf("notes for the open request = %d, want 1", got)
	}
	if status, _, _ := issueStatusAndAssignee(t, f.issueID); status != "cancelled" {
		t.Fatalf("issue = %s, want the closed status untouched", status)
	}
	f.deliverEpisodeOnlyAfterClose(t)
}

// A closed episode is not a candidate and not loadable: a second delivery does nothing.
func (f episodeFixture) deliverEpisodeOnlyAfterClose(t *testing.T) {
	t.Helper()
	before := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, f.issueID)
	runner := newDeliveryRunner(t)
	decisionPass(t, runner)
	if after := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, f.issueID); after != before {
		t.Fatalf("a later pass wrote %d comments after the close", after-before)
	}
}

// The worker finds a closed-issue episode on its own.
func TestDecisionEpisodeSystemCloseIsACandidate(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.closeIssue(t, "done")

	decisionPass(t, runner)

	if closed := f.closedEpisodes(t); len(closed) != 1 || closed[0].reason != "issue_closed" {
		t.Fatalf("closed episodes = %+v, want one closed issue_closed", closed)
	}
}

// Design §9.4: the asking agent lost its runtime binding, is archived, or is a system agent.
func TestDecisionEpisodeSystemCloseNoResumeAgent(t *testing.T) {
	cases := []struct {
		name, update, wantCancel string
	}{
		{"archived", `UPDATE agent SET archived_at = now() WHERE id = $1`, "agent_archived"},
		{"no runtime", `UPDATE agent SET runtime_id = NULL WHERE id = $1`, "agent_unavailable"},
		{"system agent", `UPDATE agent SET kind = 'system' WHERE id = $1`, "agent_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEpisodeFixture(t, false)
			f.enter(t)
			answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
			var runtimeID, kind string
			var archived *time.Time
			dbfx.QueryRow(t, `SELECT runtime_id::text, kind, archived_at FROM agent WHERE id = $1`, f.agentID).Scan(&runtimeID, &kind, &archived)
			t.Cleanup(func() {
				dbfx.Exec(t, `UPDATE agent SET runtime_id = $2, kind = $3, archived_at = $4 WHERE id = $1`, f.agentID, runtimeID, kind, archived)
			})
			dbfx.Exec(t, tc.update, f.agentID)

			f.deliverEpisodeOnly(t)

			closed := f.closedEpisodes(t)
			if len(closed) != 1 || closed[0].reason != "no_resume_agent" || closed[0].resumeTaskID != "" {
				t.Fatalf("closed episodes = %+v, want one closed no_resume_agent", closed)
			}
			if status, reason := f.requestState(t, f.requestID); status != "cancelled" || reason != tc.wantCancel {
				t.Fatalf("request = %s/%s, want cancelled/%s", status, reason, tc.wantCancel)
			}
			if woken, outcome := f.answerState(t, answerID); !woken || outcome != "skipped_ineligible" {
				t.Fatalf("answer woken=%v outcome=%q, want skipped_ineligible", woken, outcome)
			}
			status, _, _ := issueStatusAndAssignee(t, f.issueID)
			if status != "blocked" {
				t.Fatalf("issue = %s, want blocked by the system close", status)
			}
			var comment string
			dbfx.QueryRow(t, `SELECT content FROM comment WHERE issue_id = $1 AND decision_comment_kind IS NULL ORDER BY created_at DESC LIMIT 1`, f.issueID).Scan(&comment)
			if !strings.Contains(comment, "runtime binding") && tc.name == "no runtime" {
				t.Fatalf("block comment %q does not name the lost runtime binding", comment)
			}
			if got := f.continuations(t); got != 0 {
				t.Fatalf("continuation runs = %d, want none", got)
			}
		})
	}
}

// Design §9.14: the system close is one transaction. A failure while the second
// request is cancelled leaves the first request, the answer and the episode as
// they were; the retry then closes everything.
func TestDecisionEpisodeSystemCloseIsAtomic(t *testing.T) {
	f := newEpisodeFixture(t, false)
	second := f.secondIntentRequest(t)
	f.enter(t)
	answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
	f.closeIssue(t, "cancelled")

	dbfx.Exec(t, `CREATE FUNCTION che3047_fail_cancel() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.id = '`+second+`' THEN RAISE EXCEPTION 'injected cancel failure'; END IF; RETURN NEW; END $$`)
	dbfx.Exec(t, `CREATE TRIGGER che3047_fail_cancel BEFORE UPDATE OF status ON decision_request FOR EACH ROW
		WHEN (NEW.status = 'cancelled') EXECUTE FUNCTION che3047_fail_cancel()`)
	dropInjection := func() {
		dbfx.Exec(t, `DROP TRIGGER IF EXISTS che3047_fail_cancel ON decision_request`)
		dbfx.Exec(t, `DROP FUNCTION IF EXISTS che3047_fail_cancel()`)
	}
	t.Cleanup(dropInjection)

	waitID := f.onlyEpisode(t).id
	if err := testHandler.deliverDecisionItem(context.Background(), parseUUID(testWorkspaceID), parseUUID(waitID), "episode"); err == nil {
		t.Fatal("the injected failure did not surface")
	}
	if len(f.closedEpisodes(t)) != 0 {
		t.Fatal("the failed close left the episode closed")
	}
	if status, _ := f.requestState(t, f.requestID); status != "answered" {
		t.Fatalf("first request = %s, want answered (rolled back)", status)
	}
	if woken, _ := f.answerState(t, answerID); woken {
		t.Fatal("the failed close acknowledged the answer")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'cancel'`, f.issueID); n != 0 {
		t.Fatalf("cancel comments = %d, want 0", n)
	}

	dropInjection()
	f.deliverEpisodeOnly(t)
	if closed := f.closedEpisodes(t); len(closed) != 1 || closed[0].reason != "issue_closed" {
		t.Fatalf("closed episodes after the retry = %+v, want one issue_closed", closed)
	}
}

// Terra review P1 on CHE-3047: design §3.8 and §4.5 close an episode for
// no_resume_agent only when the issue ends blocked. A workflow without a
// blocked step cannot take that write, so nothing is closed or cancelled: the
// answer stays deliverable and the issue is not stranded in need_decision.
func TestDecisionEpisodeNoResumeAgentWithoutBlockedStepKeepsEpisodeOpen(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
	dbfx.Exec(t, `UPDATE issue_workflow SET steps = (SELECT jsonb_agg(s) FROM jsonb_array_elements(steps) s WHERE s ->> 'status_key' <> 'blocked')
		WHERE id = (SELECT workflow_id FROM project WHERE id = (SELECT project_id FROM issue WHERE id = $1))`, f.issueID)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, f.agentID).Scan(&runtimeID)
	t.Cleanup(func() { dbfx.Exec(t, `UPDATE agent SET runtime_id = $2 WHERE id = $1`, f.agentID, runtimeID) })
	dbfx.Exec(t, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, f.agentID)

	f.deliverEpisodeOnly(t)

	if closed := f.closedEpisodes(t); len(closed) != 0 {
		t.Fatalf("closed episodes = %+v, want none: the block cannot be written", closed)
	}
	if status, _ := f.requestState(t, f.requestID); status != "answered" {
		t.Fatalf("request = %s, want answered (nothing cancelled)", status)
	}
	if woken, _ := f.answerState(t, answerID); woken {
		t.Fatal("the answer was acknowledged although the episode did not close")
	}
	if status, _, _ := issueStatusAndAssignee(t, f.issueID); status != "need_decision" {
		t.Fatalf("issue = %s, want unchanged need_decision", status)
	}
}

// Terra review round 2 on CHE-3047: a suspended episode that cannot close
// (no resume agent, no blocked step) is an unconditional candidate. It must
// rotate behind the others (the candidate query sorts marked episodes last),
// or 100 of them starve a later ready episode.
func TestDecisionEpisodeUnclosableSuspendedDemotedBehindReadyEpisode(t *testing.T) {
	stuck := newEpisodeFixture(t, false)
	stuck.enter(t)
	stuck.answerRequest(t, stuck.requestID, -time.Minute, false)
	dbfx.Exec(t, `UPDATE issue_workflow SET steps = (SELECT jsonb_agg(s) FROM jsonb_array_elements(steps) s WHERE s ->> 'status_key' <> 'blocked')
		WHERE id = (SELECT workflow_id FROM project WHERE id = (SELECT project_id FROM issue WHERE id = $1))`, stuck.issueID)
	stuck.exhaustBudget(t)
	stuck.deliverEpisodeOnly(t)
	service.WaitRunBudgetHooks()
	if s := stuck.suspension(t); s.phase != "suspended" {
		t.Fatalf("episode = %+v, want suspended", s)
	}
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, stuck.agentID).Scan(&runtimeID)
	t.Cleanup(func() { dbfx.Exec(t, `UPDATE agent SET runtime_id = $2 WHERE id = $1`, stuck.agentID, runtimeID) })
	dbfx.Exec(t, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, stuck.agentID)

	stuck.deliverEpisodeOnly(t)

	// The candidate query sorts a marked episode behind the unmarked ones and rotates it.
	var demoted bool
	dbfx.QueryRow(t, `SELECT dispatch_refused_since IS NOT NULL FROM decision_wait WHERE issue_id = $1 AND closed_at IS NULL`, stuck.issueID).Scan(&demoted)
	if !demoted {
		t.Fatal("the unclosable suspended episode was not demoted: it keeps its slot ahead of ready episodes")
	}
}
