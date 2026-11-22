package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// decision_reask_thread (CHE-6433, thread-focus plan r3 S7): the re-ask gate and
// --reask-of / --new-topic.

const reaskKind = "decision_reask_thread"

func setReaskThread(t *testing.T, value string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = COALESCE(settings, '{}') || jsonb_build_object('decision_reask_thread', $1::text) WHERE id = $2`, value, testWorkspaceID); err != nil {
		t.Fatalf("set decision_reask_thread: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = settings - 'decision_reask_thread' WHERE id = $1`, testWorkspaceID)
	})
}

// newReaskFixture is a single-call fixture whose guard rows are removed with it.
func newReaskFixture(t *testing.T) singleCallFixture {
	t.Helper()
	fixture := newSingleCallFixture(t)
	dbfx.Cleanup(t, `DELETE FROM ask_guard_event WHERE issue_id = $1`, fixture.issueID)
	return fixture
}

// bodyAsking is the request body with its own question: the 24-hour same-ask
// rule compares questions, so two cards of one test need two.
func (fixture singleCallFixture) bodyAsking(question string) map[string]any {
	details := fixture.details
	details.Decision = question
	body := fixture.body()
	body["ask"] = details
	return body
}

func (fixture singleCallFixture) send(t *testing.T, body map[string]any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.RequestDecision, fixture.call(body))
}

// open sends a first card and returns its request id.
func (fixture singleCallFixture) open(t *testing.T, question string) string {
	t.Helper()
	response := fixture.send(t, fixture.bodyAsking(question)).Want(http.StatusCreated)
	return fmt.Sprint(response.Map()["id"])
}

func (fixture singleCallFixture) humanName(t *testing.T) string {
	t.Helper()
	var name string
	dbfx.QueryRow(t, `SELECT name FROM "user" WHERE id = $1`, fixture.humanID).Scan(&name)
	return name
}

func askBytesOf(t *testing.T, fixture singleCallFixture, body map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(body["ask"])
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type reaskCandidate struct{ id, status, question string }

// reaskRefusalText is the refusal the plan fixes, character for character.
func reaskRefusalText(member string, candidates ...reaskCandidate) string {
	var text strings.Builder
	text.WriteString("refused: decision_reask_thread\n")
	text.WriteString("This issue already has decision cards from you to " + member + ":\n")
	for _, candidate := range candidates {
		text.WriteString(fmt.Sprintf("  %s  %s  %q\n", candidate.id, candidate.status, candidate.question))
	}
	text.WriteString("Same matter (a re-ask after a rejection or a fix): add --reask-of " + candidates[0].id + ". The card goes into that card's thread.\n")
	text.WriteString("A different matter: add --new-topic. The card starts its own thread.")
	return text.String()
}

func (fixture singleCallFixture) guardRows(t *testing.T, mode string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1 AND operation_kind = $2 AND mode = $3`, fixture.issueID, reaskKind, mode)
}

func (fixture singleCallFixture) requestCount(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID)
}

func TestDecisionReaskBlockRefusesWithTheExactText(t *testing.T) {
	setReaskThread(t, "block")
	fixture := newReaskFixture(t)
	first := fixture.open(t, "Approve the reversible change?")
	comments, _, pings := fixture.counts(t)

	body := fixture.bodyAsking("Ship it on Friday?")
	response := fixture.send(t, body).Want(http.StatusConflict)
	want := reaskRefusalText(fixture.humanName(t), reaskCandidate{first, "open", "Approve the reversible change?"})
	if got := response.Map()["error"]; got != want {
		t.Errorf("refusal text:\n%v\nwant:\n%s", got, want)
	}
	if got := response.Map()["code"]; got != reaskKind {
		t.Errorf("refusal code = %v, want %s", got, reaskKind)
	}
	if n := fixture.requestCount(t); n != 1 {
		t.Errorf("requests = %d, want 1: a refusal writes no request", n)
	}
	if after, _, afterPings := fixture.counts(t); after != comments || afterPings != pings {
		t.Errorf("refusal wrote comments %d->%d or pings %d->%d", comments, after, pings, afterPings)
	}
	if n := fixture.guardRows(t, "refused"); n != 1 {
		t.Fatalf("refused rows = %d, want 1", n)
	}
	var stored string
	dbfx.QueryRow(t, `SELECT payload_sha256 FROM ask_guard_event WHERE issue_id = $1 AND operation_kind = $2`, fixture.issueID, reaskKind).Scan(&stored)
	wantKey := fixture.issueID + ":" + fixture.humanID + ":" + sha256Hex(string(askBytesOf(t, fixture, body)))
	if stored != sha256Hex(wantKey) {
		t.Errorf("payload_sha256 = %s, want sha256(%q)", stored, wantKey)
	}
	if n := fixture.guardRows(t, "block") + fixture.guardRows(t, "warn"); n != 0 {
		t.Errorf("a refusal wrote %d block or warn rows", n)
	}
}

func TestDecisionReaskBlockListsTheFiveNewestAndCutsTheQuestion(t *testing.T) {
	fixture := newReaskFixture(t)
	long := strings.Repeat("q", 100)
	var ids []string
	for i, question := range []string{"Question one?", long, "Question three?", "Question four?", "Question five?", "Question six?"} {
		ids = append(ids, fixture.open(t, question))
		// Distinct times, so "newest" is not a tie.
		dbfx.Exec(t, `UPDATE decision_request SET created_at = now() - make_interval(hours => $2) WHERE id = $1`, ids[i], 10-i)
	}
	setReaskThread(t, "block")
	response := fixture.send(t, fixture.bodyAsking("Seventh?")).Want(http.StatusConflict)
	want := reaskRefusalText(fixture.humanName(t),
		reaskCandidate{ids[5], "open", "Question six?"},
		reaskCandidate{ids[4], "open", "Question five?"},
		reaskCandidate{ids[3], "open", "Question four?"},
		reaskCandidate{ids[2], "open", "Question three?"},
		reaskCandidate{ids[1], "open", strings.Repeat("q", 80)},
	)
	if got := response.Map()["error"]; got != want {
		t.Errorf("refusal text:\n%v\nwant:\n%s", got, want)
	}
}

func TestDecisionReaskGateDoesNotFire(t *testing.T) {
	cases := []struct {
		name  string
		mode  string
		setup func(t *testing.T, fixture singleCallFixture, first string)
		body  func(fixture singleCallFixture) map[string]any
	}{
		{name: "setting absent", mode: "", body: func(f singleCallFixture) map[string]any { return f.bodyAsking("Second?") }},
		{name: "setting off", mode: "off", body: func(f singleCallFixture) map[string]any { return f.bodyAsking("Second?") }},
		{name: "new topic", mode: "block", body: func(f singleCallFixture) map[string]any {
			body := f.bodyAsking("Second?")
			body["new_topic"] = true
			return body
		}},
		{name: "earlier card older than 30 days", mode: "block", setup: func(t *testing.T, f singleCallFixture, first string) {
			dbfx.Exec(t, `UPDATE decision_request SET created_at = now() - interval '31 days' WHERE id = $1`, first)
		}, body: func(f singleCallFixture) map[string]any { return f.bodyAsking("Second?") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mode != "" {
				setReaskThread(t, tc.mode)
			}
			fixture := newReaskFixture(t)
			first := fixture.open(t, "First?")
			if tc.setup != nil {
				tc.setup(t, fixture, first)
			}
			fixture.send(t, tc.body(fixture)).Want(http.StatusCreated)
			if n := fixture.guardRows(t, "refused") + fixture.guardRows(t, "warn"); n != 0 {
				t.Errorf("guard rows = %d, want 0", n)
			}
		})
	}
	t.Run("earlier card to another member", func(t *testing.T) {
		setReaskThread(t, "block")
		fixture := newReaskFixture(t)
		other := dbfx.User(t, "Other human", "reask-"+uuid.NewString()+"@test.invalid")
		dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "owner"})
		body := fixture.bodyAsking("First?")
		body["for"] = other
		fixture.send(t, body).Want(http.StatusCreated)
		fixture.send(t, fixture.bodyAsking("Second?")).Want(http.StatusCreated)
	})
	t.Run("earlier card from another agent", func(t *testing.T) {
		setReaskThread(t, "block")
		fixture := newReaskFixture(t)
		otherAgent, otherTask := reaskSecondAgent(t, fixture)
		fixture.sendAs(t, otherAgent, otherTask, fixture.bodyAsking("First?")).Want(http.StatusCreated)
		fixture.send(t, fixture.bodyAsking("Second?")).Want(http.StatusCreated)
	})
}

// reaskSecondAgent adds a second agent with a running task on the issue.
func reaskSecondAgent(t *testing.T, fixture singleCallFixture) (agentID, taskID string) {
	t.Helper()
	agentID = createHandlerTestAgent(t, "reask-other-"+uuid.NewString()[:8], []byte("[]"))
	taskID = dbfx.Task(t, agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(t)})
	return agentID, taskID
}

func (fixture singleCallFixture) sendAs(t *testing.T, agentID, taskID string, body map[string]any) *testutil.Response {
	t.Helper()
	request := fixture.call(body)
	request.Header.Set("X-Agent-ID", agentID)
	request.Header.Set("X-Task-ID", taskID)
	return testutil.Call(t, testHandler.RequestDecision, request)
}

func TestDecisionReaskWarnCreatesTheCardAndHints(t *testing.T) {
	setReaskThread(t, "warn")
	fixture := newReaskFixture(t)
	first := fixture.open(t, "First?")
	body := fixture.bodyAsking("Second?")
	response := fixture.send(t, body).Want(http.StatusCreated)
	if n := fixture.requestCount(t); n != 2 {
		t.Fatalf("requests = %d, want 2: warn still creates the card", n)
	}
	hints, _ := response.Map()["hints"].([]any)
	want := reaskRefusalText(fixture.humanName(t), reaskCandidate{first, "open", "First?"})
	found := false
	for _, hint := range hints {
		found = found || hint == want
	}
	if !found {
		t.Errorf("hints = %v, want the refusal text %q", hints, want)
	}
	if n := fixture.guardRows(t, "warn"); n != 1 {
		t.Errorf("warn rows = %d, want 1", n)
	}
	if n := fixture.guardRows(t, "refused"); n != 0 {
		t.Errorf("refused rows = %d, want 0 under warn", n)
	}
}

// A refused call, then --new-topic in the same run, then the run ends: the
// refusal is not an unserved ask. The recovery and repair workers read block
// rows only, so nothing is noticed, repaired or pinged beyond the new card.
func TestDecisionReaskRefusedThenNewTopicLeavesNoRecovery(t *testing.T) {
	setReaskThread(t, "block")
	fixture := newReaskFixture(t)
	fixture.open(t, "First?")
	retry := fixture.bodyAsking("Second?")
	fixture.send(t, retry).Want(http.StatusConflict)
	retry["new_topic"] = true
	fixture.send(t, retry).Want(http.StatusCreated)
	inboxBefore := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	if inboxBefore == 0 {
		t.Fatal("no ping reached the inbox for the cards")
	}

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	deliverRecoveries(t)
	admitRepairs(t)
	settleRepairs(t)

	if n := dbfx.Count(t, `SELECT count(*) FROM ask_recovery_notice n JOIN ask_guard_event e ON e.id = n.event_id WHERE e.issue_id = $1`, fixture.issueID); n != 0 {
		t.Errorf("ask_recovery_notice rows = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ask_repair_root root JOIN ask_guard_event e ON e.id = root.id WHERE e.issue_id = $1`, fixture.issueID); n != 0 {
		t.Errorf("repair roots = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND id <> $2`, fixture.issueID, fixture.taskID); n != 0 {
		t.Errorf("repair runs = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); n != inboxBefore {
		t.Errorf("inbox items = %d after the run ended, want %d: the cards' pings and nothing else", n, inboxBefore)
	}
	if n := fixture.guardRows(t, "refused"); n != 1 {
		t.Errorf("refused rows = %d, want 1", n)
	}
}

type reaskState struct {
	status, withdrawReason string
	reaskOf                *string
}

func reaskStateOf(t *testing.T, requestID string) reaskState {
	t.Helper()
	var state reaskState
	dbfx.QueryRow(t, `SELECT status, COALESCE(withdraw_reason, ''), reask_of_request_id::text FROM decision_request WHERE id = $1`, requestID).Scan(&state.status, &state.withdrawReason, &state.reaskOf)
	return state
}

func TestDecisionReaskOfAnOpenCardWithdrawsItAndRepliesInItsThread(t *testing.T) {
	for _, cardThread := range []string{"off", "on"} {
		t.Run("card_thread="+cardThread, func(t *testing.T) {
			setCardThread(t, cardThread)
			setReaskThread(t, "block")
			fixture := newReaskFixture(t)
			first := fixture.open(t, "Approve the reversible change?")
			var firstComment string
			dbfx.QueryRow(t, `SELECT issue_comment_id::text FROM decision_request WHERE id = $1`, first).Scan(&firstComment)

			body := fixture.bodyAsking("Approve the reversible change?") // the same question again
			body["reask_of"] = first
			response := fixture.send(t, body).Want(http.StatusCreated)
			newID := fmt.Sprint(response.Map()["id"])

			state := reaskStateOf(t, first)
			if state.status != "cancelled" || state.withdrawReason != "Superseded by re-ask "+newID {
				t.Errorf("R = %+v, want cancelled with reason %q", state, "Superseded by re-ask "+newID)
			}
			if created := reaskStateOf(t, newID); created.status != "open" || created.reaskOf == nil || *created.reaskOf != first {
				t.Errorf("new request = %+v, want open and reask_of_request_id = %s", created, first)
			}
			var parent *string
			dbfx.QueryRow(t, `SELECT parent_id::text FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'ask'`, newID).Scan(&parent)
			if parent == nil || *parent != firstComment {
				t.Errorf("new ask parent = %v, want R's thread root %s", parent, firstComment)
			}
			var authorType, authorID, content string
			var cancelParent *string
			dbfx.QueryRow(t, `SELECT author_type, author_id::text, content, parent_id::text FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, first).Scan(&authorType, &authorID, &content, &cancelParent)
			if authorType != "agent" || authorID != fixture.agentID || content != "Decision withdrawn: Superseded by re-ask "+newID {
				t.Errorf("withdraw comment = %s %s %q", authorType, authorID, content)
			}
			if cancelParent == nil || *cancelParent != firstComment {
				t.Errorf("withdraw comment parent = %v, want %s", cancelParent, firstComment)
			}
			if n := fixture.guardRows(t, "refused") + fixture.guardRows(t, "warn"); n != 0 {
				t.Errorf("--reask-of passed the gate but wrote %d guard rows", n)
			}
		})
	}
}

func TestDecisionReaskOfAClosedCardOnlyReplies(t *testing.T) {
	fixture := newReaskFixture(t)
	first := fixture.open(t, "First?")
	dbfx.Exec(t, `UPDATE decision_request SET status = 'answered' WHERE id = $1`, first)
	body := fixture.bodyAsking("First, again?")
	body["reask_of"] = first
	response := fixture.send(t, body).Want(http.StatusCreated)
	if state := reaskStateOf(t, first); state.status != "answered" {
		t.Errorf("R status = %s, want answered and untouched", state.status)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, first); n != 0 {
		t.Errorf("a closed R got %d withdraw comments", n)
	}
	var firstComment string
	var parent *string
	dbfx.QueryRow(t, `SELECT issue_comment_id::text FROM decision_request WHERE id = $1`, first).Scan(&firstComment)
	dbfx.QueryRow(t, `SELECT parent_id::text FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'ask'`, fmt.Sprint(response.Map()["id"])).Scan(&parent)
	if parent == nil || *parent != firstComment {
		t.Errorf("new ask parent = %v, want %s", parent, firstComment)
	}
}

// The withdraw and the new ask are one commit: a failure after the withdraw
// leaves R open and no new ask behind.
func TestDecisionReaskFailureAfterTheWithdrawLeavesROpen(t *testing.T) {
	fixture := newReaskFixture(t)
	first := fixture.open(t, "First?")
	comments, messages, pings := fixture.counts(t)
	useRequestCoreStarter(t, &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: MarkDecisionPingNotified", fail: &pgconn.PgError{Code: "23514", Message: "injected ping receipt failure"}})
	body := fixture.bodyAsking("First, again?")
	body["reask_of"] = first
	if status := fixture.send(t, body).Code; status < 500 || status >= 600 {
		t.Fatalf("status = %d, want a 5xx", status)
	}
	if state := reaskStateOf(t, first); state.status != "open" || state.withdrawReason != "" {
		t.Errorf("R = %+v, want open and not withdrawn", state)
	}
	if n := fixture.requestCount(t); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, first); n != 0 {
		t.Errorf("withdraw comments = %d, want 0", n)
	}
	if afterComments, afterMessages, afterPings := fixture.counts(t); afterComments != comments || afterMessages != messages || afterPings != pings {
		t.Errorf("counts changed: comments %d->%d, messages %d->%d, pings %d->%d", comments, afterComments, messages, afterMessages, pings, afterPings)
	}
}

func TestDecisionReaskByAnotherAgentWithdrawsAsThatAgent(t *testing.T) {
	setReaskThread(t, "block")
	fixture := newReaskFixture(t)
	first := fixture.open(t, "First?")
	otherAgent, otherTask := reaskSecondAgent(t, fixture)
	body := fixture.bodyAsking("First, again?")
	body["reask_of"] = first
	response := fixture.sendAs(t, otherAgent, otherTask, body).Want(http.StatusCreated)
	newID := fmt.Sprint(response.Map()["id"])

	var askingAgent string
	dbfx.QueryRow(t, `SELECT asking_agent_id::text FROM decision_request WHERE id = $1`, first).Scan(&askingAgent)
	if askingAgent != fixture.agentID {
		t.Errorf("R asking agent = %s, want %s: it stays", askingAgent, fixture.agentID)
	}
	if state := reaskStateOf(t, first); state.status != "cancelled" || state.withdrawReason != "Superseded by re-ask "+newID {
		t.Errorf("R = %+v, want cancelled by the re-ask", state)
	}
	var authorID string
	var taskID *string
	dbfx.QueryRow(t, `SELECT author_id::text, source_task_id::text FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, first).Scan(&authorID, &taskID)
	if authorID != otherAgent || taskID == nil || *taskID != otherTask {
		t.Errorf("withdraw comment author = %s task = %v, want the re-asking agent %s and its task %s", authorID, taskID, otherAgent, otherTask)
	}
	var newAgent string
	dbfx.QueryRow(t, `SELECT asking_agent_id::text FROM decision_request WHERE id = $1`, newID).Scan(&newAgent)
	if newAgent != otherAgent {
		t.Errorf("new request asking agent = %s, want %s", newAgent, otherAgent)
	}
}

func TestDecisionReaskRefusals(t *testing.T) {
	fixture := newReaskFixture(t)
	first := fixture.open(t, "First?")
	reask := func(target string) map[string]any {
		body := fixture.bodyAsking("Again?")
		body["reask_of"] = target
		return body
	}
	t.Run("both flags", func(t *testing.T) {
		body := reask(first)
		body["new_topic"] = true
		fixture.send(t, body).Want(http.StatusBadRequest)
	})
	t.Run("not a uuid", func(t *testing.T) {
		fixture.send(t, reask("not-a-uuid")).Want(http.StatusBadRequest)
	})
	t.Run("unknown request", func(t *testing.T) {
		fixture.send(t, reask(uuid.NewString())).Want(http.StatusNotFound)
	})
	t.Run("another issue", func(t *testing.T) {
		elsewhere := newSingleCallFixture(t)
		other := elsewhere.open(t, "Elsewhere?")
		response := fixture.send(t, reask(other)).Want(http.StatusBadRequest)
		if got := response.Map()["code"]; got != "reask_issue_mismatch" {
			t.Errorf("code = %v, want reask_issue_mismatch", got)
		}
	})
	t.Run("another member", func(t *testing.T) {
		other := dbfx.User(t, "Other human", "reask-"+uuid.NewString()+"@test.invalid")
		dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "owner"})
		body := fixture.bodyAsking("To someone else?")
		body["for"] = other
		otherCard := fmt.Sprint(fixture.send(t, body).Want(http.StatusCreated).Map()["id"])
		response := fixture.send(t, reask(otherCard)).Want(http.StatusBadRequest)
		if got := response.Map()["code"]; got != "reask_target_mismatch" {
			t.Errorf("code = %v, want reask_target_mismatch", got)
		}
	})
	t.Run("server request", func(t *testing.T) {
		serverCard := fixture.open(t, "Opened by the platform?")
		dbfx.Exec(t, `UPDATE decision_request SET request_fingerprint = 'server:reask-test' WHERE id = $1`, serverCard)
		response := fixture.send(t, reask(serverCard)).Want(http.StatusConflict)
		if got := response.Map()["code"]; got != "reask_server_request" {
			t.Errorf("code = %v, want reask_server_request", got)
		}
		if state := reaskStateOf(t, serverCard); state.status != "open" {
			t.Errorf("a platform request was %s, want it left open", state.status)
		}
	})
	t.Run("with a parent", func(t *testing.T) {
		body := reask(first)
		body["parent_id"] = fixture.sourceID
		fixture.send(t, body).Want(http.StatusBadRequest)
	})
	t.Run("with an open intent", func(t *testing.T) {
		openLaneIntent(t, fixture.intentFixture, fixture.mention())
		body := reask(first)
		body["new_dm"] = true
		fixture.send(t, body).Want(http.StatusBadRequest)
		if state := reaskStateOf(t, first); state.status != "open" {
			t.Errorf("R was %s after a refused re-ask", state.status)
		}
	})
}

// Two re-asks of one card: the second waits for the first's locks, then
// replays it (same body) or is refused (another body). Never a deadlock, never
// two new cards.
func TestDecisionReaskConcurrentOnTheSameCard(t *testing.T) {
	for _, same := range []bool{true, false} {
		name := map[bool]string{true: "same body", false: "different body"}[same]
		t.Run(name, func(t *testing.T) {
			fixture := newReaskFixture(t)
			first := fixture.open(t, "First?")
			bodies := []map[string]any{fixture.bodyAsking("Again?"), fixture.bodyAsking("Again?")}
			if !same {
				bodies[1] = fixture.bodyAsking("Again, differently?")
			}
			var requests []*http.Request
			for _, body := range bodies {
				body["reask_of"] = first
				requests = append(requests, fixture.call(body))
			}
			codes := make([]int, 2)
			var wg sync.WaitGroup
			for i := range requests {
				wg.Add(1)
				go func() {
					defer wg.Done()
					recorder := httptest.NewRecorder()
					testHandler.RequestDecision(recorder, requests[i])
					codes[i] = recorder.Code
				}()
			}
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("two re-asks of one card did not finish: deadlock")
			}
			created, other := 0, 0
			for _, code := range codes {
				switch code {
				case http.StatusCreated:
					created++
				case http.StatusOK, http.StatusConflict:
					other++
				}
			}
			if created != 1 || other != 1 {
				t.Fatalf("statuses = %v, want one 201 and one 200 or 409", codes)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND reask_of_request_id = $2`, fixture.issueID, first); n != 1 {
				t.Errorf("re-asks of the card = %d, want 1", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND status = 'open'`, fixture.issueID); n != 1 {
				t.Errorf("open cards = %d, want 1", n)
			}
		})
	}
}

// A re-ask of a card that a re-ask already replaced is refused; the newer card
// is the one to name.
func TestDecisionReaskOfASupersededCardPointsAtTheSuccessor(t *testing.T) {
	fixture := newReaskFixture(t)
	first := fixture.open(t, "First?")
	body := fixture.bodyAsking("Again?")
	body["reask_of"] = first
	successor := fmt.Sprint(fixture.send(t, body).Want(http.StatusCreated).Map()["id"])
	body = fixture.bodyAsking("Again, differently?")
	body["reask_of"] = first
	response := fixture.send(t, body).Want(http.StatusConflict)
	if got := response.Map()["code"]; got != "reask_superseded" || !strings.Contains(fmt.Sprint(response.Map()["error"]), successor) {
		t.Errorf("response = %s, want reask_superseded naming %s", response.Text(), successor)
	}
}

// Neither the gate nor the re-ask path may add a level=WARN line (plan r3 S7).
func TestDecisionReaskLogsNoWarnLine(t *testing.T) {
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	fixture := newReaskFixture(t)
	first := fixture.open(t, "First?")
	setReaskThread(t, "block")
	fixture.send(t, fixture.bodyAsking("Second?")).Want(http.StatusConflict)
	setReaskThread(t, "warn")
	fixture.send(t, fixture.bodyAsking("Third?")).Want(http.StatusCreated)
	body := fixture.bodyAsking("Fourth?")
	body["reask_of"] = first
	fixture.send(t, body).Want(http.StatusCreated)
	if strings.Contains(buffer.String(), "level=WARN") {
		t.Errorf("a level=WARN line was logged:\n%s", buffer.String())
	}
}

func TestValidateReaskThreadSetting(t *testing.T) {
	for _, mode := range []string{"off", "warn", "block"} {
		if err := validateReaskThreadSetting(map[string]any{"decision_reask_thread": mode}); err != nil {
			t.Errorf("%s refused: %v", mode, err)
		}
	}
	if err := validateReaskThreadSetting(map[string]any{}); err != nil {
		t.Errorf("an absent key refused: %v", err)
	}
	for _, bad := range []any{"on", "move", "", nil, 1, true} {
		if err := validateReaskThreadSetting(map[string]any{"decision_reask_thread": bad}); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
