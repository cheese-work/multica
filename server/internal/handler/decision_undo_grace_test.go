package handler

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A8 (CHE-5480 plan r4, section 3.11): server undo grace. A card answer stores
// undo_until = now + decision_undo_grace and holds effective_at / wake_at until
// then; POST /api/decision-requests/{id}/undo withdraws it before it wakes anyone.

// undoCall posts the undo for the card's request as the given member.
func undoCall(test *testing.T, fixture askedRecordFixture, userID string) *testutil.Response {
	test.Helper()
	id := uuidToString(fixture.decision.ID)
	request := withURLParam(newRequest(http.MethodPost, "/api/decision-requests/"+id+"/undo", nil), "id", id)
	request.Header.Set("X-User-ID", userID)
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	return testutil.Call(test, testHandler.UndoDecisionRequest, request)
}

// readAnswerAsMember reads the request as its member and returns the answer object, if any.
func readAnswerAsMember(test *testing.T, fixture askedRecordFixture) map[string]any {
	test.Helper()
	id := uuidToString(fixture.decision.ID)
	request := withURLParam(newRequest(http.MethodGet, "/api/decisions/"+id, nil), "id", id)
	request.Header.Set("X-User-ID", fixture.humanID)
	var read struct {
		Answer map[string]any `json:"answer"`
	}
	testutil.Call(test, testHandler.GetDecision, request).Want(http.StatusOK).JSON(&read)
	return read.Answer
}

// elapse moves the answer's clock back by seconds, as if that much time had passed.
func elapse(test *testing.T, answerID any, seconds int) {
	test.Helper()
	dbfx.Exec(test, `UPDATE decision_answer SET
		undo_until = undo_until - make_interval(secs => $2),
		effective_at = effective_at - make_interval(secs => $2),
		wake_at = wake_at - make_interval(secs => $2),
		created_at = created_at - make_interval(secs => $2)
		WHERE id = $1`, answerID, seconds)
}

func answerWithGrace(test *testing.T, class string, graceSeconds int) (askedRecordFixture, decisionRecordResult) {
	test.Helper()
	resetDecisionLease(test)
	setWorkspaceDecisionSettings(test, `{"decision_undo_grace":`+strconv.Itoa(graceSeconds)+`}`)
	fixture := newCardFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = $2 WHERE id = $1`, fixture.decision.ID, class)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
	if err != nil {
		test.Fatal(err)
	}
	return fixture, result
}

func wakeTasks(test *testing.T, fixture askedRecordFixture) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running')`, fixture.issueID, fixture.agentID)
}

// Core red test: undo at +2 s leaves no task after the worker runs at +10 s.
func TestUndoPreventsWake(test *testing.T) {
	fixture, result := answerWithGrace(test, "reversible", 5)
	if !result.Answer.UndoUntil.Valid {
		test.Fatal("card answer stored no undo_until")
	}
	if held := result.Answer.UndoUntil.Time.Sub(result.Answer.CreatedAt.Time); held < 4*time.Second || held > 6*time.Second {
		test.Fatalf("undo_until - created_at = %s, want about 5s", held)
	}
	if result.Answer.EffectiveAt.Time.Before(result.Answer.UndoUntil.Time) {
		test.Fatalf("effective_at %s is before undo_until %s", result.Answer.EffectiveAt.Time, result.Answer.UndoUntil.Time)
	}
	if !result.Answer.WakeAt.Valid || !result.Answer.WakeAt.Time.Equal(result.Answer.EffectiveAt.Time) {
		test.Fatalf("wake_at %v != effective_at %v", result.Answer.WakeAt, result.Answer.EffectiveAt)
	}

	elapse(test, result.Answer.ID, 2)
	if open := readAnswerAsMember(test, fixture); open["undo_until"] == nil {
		test.Fatalf("read at +2 s carries no undo_until: %v", open)
	}
	undoCall(test, fixture, fixture.humanID).Want(http.StatusOK)

	var reason string
	var disclaimed bool
	dbfx.QueryRow(test, `SELECT COALESCE(disclaim_reason, ''), disclaimed_at IS NOT NULL FROM decision_answer WHERE id = $1`, result.Answer.ID).Scan(&reason, &disclaimed)
	if !disclaimed || reason != "undo" {
		test.Fatalf("answer disclaimed=%v reason=%q, want true/undo", disclaimed, reason)
	}
	if status := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, fixture.decision.ID); status != 1 {
		test.Fatal("request is not open again")
	}

	elapse(test, result.Answer.ID, 8)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if tasks := wakeTasks(test, fixture); tasks != 0 {
		test.Fatalf("%d tasks after an undone answer, want 0", tasks)
	}
}

func TestNoUndoEnqueuesOneTaskAfterTheGrace(test *testing.T) {
	fixture, result := answerWithGrace(test, "reversible", 5)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if tasks := wakeTasks(test, fixture); tasks != 0 {
		test.Fatalf("%d tasks inside the grace, want 0", tasks)
	}
	elapse(test, result.Answer.ID, 5)
	decisionPass(test, runner)
	decisionPass(test, runner)
	if tasks := wakeTasks(test, fixture); tasks != 1 {
		test.Fatalf("%d tasks at +5 s, want 1", tasks)
	}
}

func TestUndoAfterUndoUntilIsExpired(test *testing.T) {
	fixture, result := answerWithGrace(test, "reversible", 5)
	elapse(test, result.Answer.ID, 6)
	if body := undoCall(test, fixture, fixture.humanID).Want(http.StatusConflict).Text(); !strings.Contains(body, "undo_expired") {
		test.Fatalf("body %q lacks undo_expired", body)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NULL`, result.Answer.ID) != 1 {
		test.Fatal("an expired undo withdrew the answer")
	}
}

func TestSpendAnswerKeepsItsClassHoldPastTheGrace(test *testing.T) {
	fixture, result := answerWithGrace(test, "spend", 5)
	held := result.Answer.EffectiveAt.Time.Sub(result.Answer.CreatedAt.Time)
	if held < 9*time.Minute+50*time.Second || held > 10*time.Minute+10*time.Second {
		test.Fatalf("effective_at - created_at = %s, want about 10 min", held)
	}
	elapse(test, result.Answer.ID, 6)
	if answer := readAnswerAsMember(test, fixture); answer["undo_until"] != nil {
		test.Fatalf("read at +6 s still carries undo_until: %v", answer["undo_until"])
	}
	undoCall(test, fixture, fixture.humanID).Want(http.StatusConflict)
}

func TestUndoByAnotherMemberIsForbidden(test *testing.T) {
	fixture, result := answerWithGrace(test, "reversible", 5)
	other := dbfx.User(test, "Other member", "undo-other-"+time.Now().Format("150405.000000")+"@test.invalid")
	dbfx.Insert(test, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "owner"})
	undoCall(test, fixture, other).Want(http.StatusForbidden)
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NULL`, result.Answer.ID) != 1 {
		test.Fatal("another member's undo withdrew the answer")
	}
}

// With the setting absent (decision_screen off), a reversible answer takes effect at once, as before.
func TestUndoGraceOffKeepsTodaysBehaviour(test *testing.T) {
	resetDecisionLease(test)
	fixture := newCardFixture(test)
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
	if err != nil {
		test.Fatal(err)
	}
	if result.Answer.UndoUntil.Valid {
		test.Fatalf("undo_until = %v with the grace off", result.Answer.UndoUntil)
	}
	if result.Answer.EffectiveAt.Time.After(time.Now().Add(time.Second)) {
		test.Fatalf("effective_at %s is not immediate", result.Answer.EffectiveAt.Time)
	}
	undoCall(test, fixture, fixture.humanID).Want(http.StatusConflict)
}

// Round 1 P2: an undone answer never woke anyone, so its retraction is suppressed (plan r4, 3.11).
func TestUndoSuppressesTheRetraction(test *testing.T) {
	fixture, result := answerWithGrace(test, "reversible", 5)
	undoCall(test, fixture, fixture.humanID).Want(http.StatusOK)
	var outcome *string
	var delivered bool
	dbfx.QueryRow(test, `SELECT retraction_outcome, retraction_delivered_at IS NOT NULL FROM decision_answer WHERE id = $1`, result.Answer.ID).Scan(&outcome, &delivered)
	if outcome == nil || *outcome != "suppressed_agent_veto" || !delivered {
		test.Fatalf("retraction outcome=%v delivered=%v, want a suppressed retraction", outcome, delivered)
	}
}

// batchAnswerWithGrace answers a batch card covering two reversible cards under the grace.
func batchAnswerWithGrace(test *testing.T, graceSeconds int) batchFixture {
	test.Helper()
	resetDecisionLease(test)
	setWorkspaceDecisionSettings(test, `{"decision_undo_grace":`+strconv.Itoa(graceSeconds)+`}`)
	fixture := newBatchFixture(test, 2)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.batch.taskID, fixture.batch.chatTaskID)
	for _, card := range fixture.covered {
		dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'reversible' WHERE id = $1`, card.decision.ID)
		dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, card.taskID, card.chatTaskID)
	}
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'reversible' WHERE id = $1`, fixture.batch.decision.ID)
	if _, err := fixture.answerAcknowledged(test, "Option B"); err != nil {
		test.Fatal(err)
	}
	return fixture
}

func elapseCovered(test *testing.T, fixture batchFixture, seconds int) {
	test.Helper()
	for _, card := range append([]askedRecordFixture{fixture.batch}, fixture.covered...) {
		dbfx.Exec(test, `UPDATE decision_answer SET
			undo_until = undo_until - make_interval(secs => $2),
			effective_at = effective_at - make_interval(secs => $2),
			wake_at = wake_at - make_interval(secs => $2),
			created_at = created_at - make_interval(secs => $2)
			WHERE decision_request_id = $1`, card.decision.ID, seconds)
	}
}

// Round 1 P1: a covered answer holds its wake for the same grace as the batch answer.
func TestBatchUndoPreventsCoveredWake(test *testing.T) {
	fixture := batchAnswerWithGrace(test, 5)
	for _, card := range fixture.covered {
		if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND undo_until IS NOT NULL AND wake_at >= undo_until AND effective_at >= undo_until`, card.decision.ID) != 1 {
			test.Fatalf("covered card %s holds no undo grace", uuidToString(card.decision.ID))
		}
	}
	elapseCovered(test, fixture, 2)
	undoCall(test, fixture.batch, fixture.batch.humanID).Want(http.StatusOK)
	for _, card := range fixture.covered {
		var outcome *string
		dbfx.QueryRow(test, `SELECT retraction_outcome FROM decision_answer WHERE decision_request_id = $1 AND disclaimed_at IS NOT NULL`, card.decision.ID).Scan(&outcome)
		if outcome == nil || *outcome != "suppressed_agent_veto" {
			test.Fatalf("covered card %s retraction outcome = %v, want suppressed", uuidToString(card.decision.ID), outcome)
		}
	}
	elapseCovered(test, fixture, 8)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	decisionPass(test, runner)
	for _, card := range fixture.covered {
		if tasks := wakeTasks(test, card); tasks != 0 {
			test.Fatalf("covered agent has %d tasks after the batch was undone, want 0", tasks)
		}
	}
}

func TestBatchWithoutUndoWakesCoveredAgentsAfterTheGrace(test *testing.T) {
	fixture := batchAnswerWithGrace(test, 5)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	for _, card := range fixture.covered {
		if tasks := wakeTasks(test, card); tasks != 0 {
			test.Fatalf("covered agent has %d tasks inside the grace, want 0", tasks)
		}
	}
	elapseCovered(test, fixture, 5)
	decisionPass(test, runner)
	decisionPass(test, runner)
	for _, card := range fixture.covered {
		if tasks := wakeTasks(test, card); tasks != 1 {
			test.Fatalf("covered agent has %d tasks after the grace, want 1", tasks)
		}
	}
}
