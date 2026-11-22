package handler

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/intakehold"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7073 (E3, rows 1 and 2): a decision delivery on a held issue dispatches
// nothing, settles skipped_ineligible and is not retried. The cards below go
// through the real create, park, answer and worker path.

// heldCard is a plain or action request asked by a task on an issue the hold
// owns: the issue was in backlog with Readiness V before the card parked it.
type heldCard struct {
	askedRecordFixture
	hold        holdFixture
	readinessID string
}

const heldReadiness = "ready"

func newHeldCard(test *testing.T, class, kind string, edit func(entry map[string]any)) heldCard {
	test.Helper()
	resetDecisionLease(test)
	readinessID := ensureParkCatalog(test)
	hold := newHoldFixture(test, edit)
	// The project's workflow lists need_decision, the step a card parks to.
	flow := dbfx.Insert(test, "issue_workflow", testutil.Cols{"workspace_id": testWorkspaceID, "name": "hold " + hold.projectID, "initial_status_key": "backlog", "steps": `[{"status_key":"backlog"},{"status_key":"in_progress"},{"status_key":"need_decision"},{"status_key":"blocked"}]`})
	dbfx.Exec(test, `UPDATE project SET workflow_id = $1 WHERE id = $2`, flow, hold.projectID)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE project SET workflow_id = NULL WHERE id = $1`, hold.projectID)
		dbfx.Exec(test, `DELETE FROM issue_workflow WHERE id = $1`, flow)
	})
	core := newRequestCoreFixture(test, false)
	dbfx.Exec(test, `UPDATE issue SET status = 'backlog', project_id = $1, properties = jsonb_build_object($2::text, $3::text) WHERE id = $4`, hold.projectID, readinessID, heldReadiness, core.issueID)
	var err error
	if core.issue, err = testHandler.Queries.GetIssue(context.Background(), parseUUID(core.issueID)); err != nil {
		test.Fatal(err)
	}
	input := core.input()
	input.EscalationClass = class
	result, err := testHandler.createDecisionRequest(core.request(), core.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	if result.ParkedActivity == nil {
		test.Fatal("the card did not park the issue")
	}
	core.issue = result.ParkedIssue
	fixture := newAskedFixtureFrom(test, core, result)
	dbfx.Exec(test, `UPDATE decision_request SET options = $2::jsonb, ask_json = $3::jsonb WHERE id = $1`, fixture.decision.ID, cardOptions, cardAskJSON)
	dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, fixture.intentID)
	if kind == "action" {
		dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, fixture.decision.ID)
		fixture.decision.Kind = "action"
	}
	// The task that sent the request completes before the answer.
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE status IN ('queued', 'running') AND issue_id = $1`, fixture.issueID)
	})
	card := heldCard{fixture, hold, readinessID}
	card.wantParked(test)
	return card
}

// newAskedFixtureFrom is newAskedRecordFixture for a request already created.
func newAskedFixtureFrom(test *testing.T, core requestCoreFixture, result decisionRequestResult) askedRecordFixture {
	test.Helper()
	messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": core.sessionID, "role": "user", "content": "  Yes.\nKeep the exact words.  ", "sent_via": "interactive_jwt"})
	chatTaskID := dbfx.Task(test, core.agentID, testutil.Cols{"status": "running", "chat_session_id": core.sessionID, "runtime_id": handlerTestRuntimeID(test)})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, chatTaskID)
	return askedRecordFixture{core, result.Request, uuidToString(result.Comment.ID), messageID, chatTaskID}
}

func (card heldCard) state(test *testing.T) (status, readiness string, parks int) {
	test.Helper()
	dbfx.QueryRow(test, `SELECT status, COALESCE(properties ->> $1, ''), (SELECT count(*) FROM issue_decision_park WHERE issue_id = issue.id) FROM issue WHERE id = $2`, card.readinessID, card.issueID).Scan(&status, &readiness, &parks)
	return
}

func (card heldCard) wantParked(test *testing.T) {
	test.Helper()
	status, readiness, parks := card.state(test)
	if status != "need_decision" || readiness != "gate" || parks != 1 {
		test.Fatalf("parked state = %s/%s/%d parks, want need_decision/gate/1", status, readiness, parks)
	}
	wantHold(test, card.issueID, true, true, intakehold.ReasonHeld)
}

// wantBackInBacklog is the unpark: status and Readiness restored, park row gone.
func (card heldCard) wantBackInBacklog(test *testing.T) {
	test.Helper()
	status, readiness, parks := card.state(test)
	if status != "backlog" || readiness != heldReadiness || parks != 0 {
		test.Fatalf("issue = %s/%s/%d parks, want backlog/%s/0", status, readiness, parks, heldReadiness)
	}
}

func (card heldCard) tasks(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, card.issueID)
}

func (card heldCard) answer(test *testing.T) {
	test.Helper()
	input := decisionCardAnswerInput{Option: "Option B"}
	if card.decision.Kind == "action" {
		input = decisionCardAnswerInput{Text: "done"}
	}
	if _, err := card.answerOnCard(input); err != nil {
		test.Fatal(err)
	}
}

// pass is one ProcessPass of a fresh worker. It clears the lease first: a pass
// keeps it for 30 seconds, and a second pass must run, not be refused.
func (card heldCard) pass(test *testing.T) {
	test.Helper()
	dbfx.Exec(test, `DELETE FROM decision_worker_lease`)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
}

type answerWake struct {
	Woken   bool
	Outcome string
}

func (card heldCard) answerWakes(test *testing.T) []answerWake {
	test.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT woken_at IS NOT NULL, COALESCE(wake_outcome, '') FROM decision_answer WHERE decision_request_id = $1`, card.decision.ID)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	var wakes []answerWake
	for rows.Next() {
		var wake answerWake
		if err := rows.Scan(&wake.Woken, &wake.Outcome); err != nil {
			test.Fatal(err)
		}
		wakes = append(wakes, wake)
	}
	return wakes
}

func (card heldCard) wantAnswerWake(test *testing.T, outcome string) {
	test.Helper()
	wakes := card.answerWakes(test)
	if len(wakes) != 1 || !wakes[0].Woken || wakes[0].Outcome != outcome {
		test.Fatalf("answer wake = %+v, want one woken with %s", wakes, outcome)
	}
}

type receiptRow struct {
	Status, Outcome, LastError string
	Attempts                   int
}

func (card heldCard) receipts(test *testing.T, eventType string) []receiptRow {
	test.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT r.status, COALESCE(r.outcome, ''), COALESCE(r.last_error, ''), r.attempt_count
		FROM decision_wake_receipt r JOIN decision_wake_event e ON e.id = r.event_id WHERE e.request_id = $1 AND e.type = $2`, card.decision.ID, eventType)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	var found []receiptRow
	for rows.Next() {
		var row receiptRow
		if err := rows.Scan(&row.Status, &row.Outcome, &row.LastError, &row.Attempts); err != nil {
			test.Fatal(err)
		}
		found = append(found, row)
	}
	return found
}

func (card heldCard) wantSkippedReceipts(test *testing.T, eventType string) {
	test.Helper()
	found := card.receipts(test, eventType)
	if len(found) == 0 {
		test.Fatalf("no %s receipt", eventType)
	}
	for _, row := range found {
		if row != (receiptRow{Status: "skipped_ineligible"}) {
			test.Fatalf("%s receipt = %+v, want skipped_ineligible, no outcome, 0 attempts, no last_error", eventType, row)
		}
	}
}

// digest fingerprints every row a delivery could touch, so "changes nothing"
// is one comparison.
func (card heldCard) digest(test *testing.T) string {
	test.Helper()
	var digest string
	dbfx.QueryRow(test, `SELECT concat_ws(' ',
		'issue=' || md5(COALESCE((SELECT string_agg(i::text, '|') FROM issue i WHERE i.id = $1), '')),
		'park=' || md5(COALESCE((SELECT string_agg(p::text, '|' ORDER BY p.issue_id) FROM issue_decision_park p WHERE p.issue_id = $1), '')),
		'request=' || md5(COALESCE((SELECT string_agg(q::text, '|' ORDER BY q.id) FROM decision_request q WHERE q.issue_id = $1), '')),
		'answer=' || md5(COALESCE((SELECT string_agg(a::text, '|' ORDER BY a.id) FROM decision_answer a JOIN decision_request q ON q.id = a.decision_request_id WHERE q.issue_id = $1), '')),
		'receipt=' || md5(COALESCE((SELECT string_agg(r::text, '|' ORDER BY r.id) FROM decision_wake_receipt r JOIN decision_wake_event e ON e.id = r.event_id JOIN decision_request q ON q.id = e.request_id WHERE q.issue_id = $1), '')),
		'task=' || md5(COALESCE((SELECT string_agg(t::text, '|' ORDER BY t.id) FROM agent_task_queue t WHERE t.issue_id = $1), '')))`, card.issueID).Scan(&digest)
	return digest
}

// deliveryRows is the digest without the issue and request rows, which the
// wait guard touches on any pass; it holds what a delivery would change.
func (card heldCard) deliveryRows(test *testing.T) string {
	test.Helper()
	var kept []string
	for _, part := range strings.Fields(card.digest(test)) {
		if !strings.HasPrefix(part, "issue=") && !strings.HasPrefix(part, "request=") {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " ")
}

func (card heldCard) wantNoPendingReceipts(test *testing.T) {
	test.Helper()
	ids, err := testHandler.Queries.ListDecisionWakeReceiptCandidates(context.Background(), 100)
	if err != nil {
		test.Fatal(err)
	}
	for _, id := range ids {
		var request string
		dbfx.QueryRow(test, `SELECT e.request_id::text FROM decision_wake_receipt r JOIN decision_wake_event e ON e.id = r.event_id WHERE r.id = $1`, id).Scan(&request)
		if request == uuidToString(card.decision.ID) {
			test.Fatalf("receipt %s of the request is still a candidate", uuidToString(id))
		}
	}
}

// slogCapture records every slog record while a test runs.
type slogCapture struct {
	mu      sync.Mutex
	records []map[string]any
}

func (capture *slogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (capture *slogCapture) WithAttrs([]slog.Attr) slog.Handler       { return capture }
func (capture *slogCapture) WithGroup(string) slog.Handler            { return capture }
func (capture *slogCapture) Handle(_ context.Context, record slog.Record) error {
	entry := map[string]any{"msg": record.Message}
	record.Attrs(func(attr slog.Attr) bool {
		entry[attr.Key] = attr.Value.Any()
		return true
	})
	capture.mu.Lock()
	capture.records = append(capture.records, entry)
	capture.mu.Unlock()
	return nil
}

func recordHoldLogs(test *testing.T) *slogCapture {
	test.Helper()
	capture := &slogCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	test.Cleanup(func() { slog.SetDefault(previous) })
	return capture
}

func (capture *slogCapture) lines(message string) []map[string]any {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	var found []map[string]any
	for _, entry := range capture.records {
		if entry["msg"] == message {
			found = append(found, entry)
		}
	}
	return found
}

func (capture *slogCapture) wantSite(test *testing.T, message, site string, count int) {
	test.Helper()
	lines := capture.lines(message)
	if len(lines) != count {
		test.Fatalf("%s lines = %v, want %d", message, lines, count)
	}
	for _, line := range lines {
		if line["site"] != site {
			test.Fatalf("%s line = %v, want site %s", message, line, site)
		}
	}
}

// Row 1, case (a): a plain reversible request. The answer is acknowledged
// skipped_ineligible, the issue unparks, and no task is queued.
func TestHeldIssuePlainAnswerDeliversNothing(test *testing.T) {
	card := newHeldCard(test, "reversible", "", nil)
	logs := recordHoldLogs(test)
	tasks := card.tasks(test)
	card.answer(test)
	card.pass(test)
	card.wantBackInBacklog(test)
	card.wantAnswerWake(test, "skipped_ineligible")
	if after := card.tasks(test); after != tasks {
		test.Fatalf("tasks %d -> %d, want none queued", tasks, after)
	}
	logs.wantSite(test, "intake_hold_delivery_skipped", "decision_wake", 1)
	settled := card.digest(test)
	card.pass(test)
	if card.digest(test) != settled {
		test.Fatal("a second pass changed a row")
	}
}

// Row 2, case (b): an action request answered by the person.
func TestHeldIssueActionAnswerReceiptsSkipped(test *testing.T) {
	card := newHeldCard(test, "reversible", "action", nil)
	logs := recordHoldLogs(test)
	tasks := card.tasks(test)
	card.answer(test)
	card.pass(test)
	card.wantSkippedReceipts(test, "answered")
	card.wantAnswerWake(test, "skipped_ineligible")
	card.wantBackInBacklog(test)
	if after := card.tasks(test); after != tasks {
		test.Fatalf("tasks %d -> %d, want none queued", tasks, after)
	}
	logs.wantSite(test, "intake_hold_delivery_skipped", "decision_receipt", len(card.receipts(test, "answered")))
	settled := card.digest(test)
	card.pass(test)
	card.wantNoPendingReceipts(test)
	if card.digest(test) != settled {
		test.Fatal("a second pass changed a row")
	}
}

// Row 2, case (c): every non-answer event type of an action request. The
// production code at this head writes only answered and retracted events, so
// each event row is inserted on a real parked request (cancelled with the
// cancel reason its type needs).
func TestHeldIssueActionNonAnswerEventsSkipped(test *testing.T) {
	cancels := map[string]string{"expired": "expired", "withdrawn_human": "agent_withdrawn", "withdrawn_agent": "agent_withdrawn", "reask": "human_removed", "requester_archived": "agent_archived"}
	for _, eventType := range []string{"expired", "withdrawn_human", "withdrawn_agent", "reask", "requester_archived", "retracted"} {
		test.Run(eventType, func(test *testing.T) {
			card := newHeldCard(test, "reversible", "action", nil)
			ctx := context.Background()
			var answer pgtype.UUID
			if eventType == "retracted" {
				card.answer(test)
				dbfx.QueryRow(test, `SELECT id FROM decision_answer WHERE decision_request_id = $1`, card.decision.ID).Scan(&answer)
				dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE decision_request_id = $1`, card.decision.ID)
				dbfx.Exec(test, `UPDATE decision_wake_event SET superseded_at = clock_timestamp(), superseded_reason = 'answer_disclaimed' WHERE request_id = $1`, card.decision.ID)
			} else {
				dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = $2 WHERE id = $1`, card.decision.ID, cancels[eventType])
			}
			request, err := testHandler.Queries.GetDecisionRequestForWake(ctx, card.decision.ID)
			if err != nil {
				test.Fatal(err)
			}
			if err := testHandler.recordDecisionWakeEvent(ctx, testHandler.Queries, request, eventType, answer, parseUUID(card.parentID)); err != nil {
				test.Fatal(err)
			}
			card.wantParked(test)
			tasks := card.tasks(test)
			card.pass(test)
			if eventType == "retracted" {
				// The older answered receipt settles in the first pass and blocks this
				// one in the same pass (HasOlderPendingDecisionWakeReceipt).
				card.pass(test)
			}
			card.wantSkippedReceipts(test, eventType)
			card.wantBackInBacklog(test)
			if after := card.tasks(test); after != tasks {
				test.Fatalf("tasks %d -> %d, want none queued", tasks, after)
			}
			settled := card.digest(test)
			card.pass(test)
			if card.digest(test) != settled {
				test.Fatal("a second pass changed a row")
			}
		})
	}
}

// exemptSpend lets a person's answer to a spend request end the hold.
func exemptSpend(entry map[string]any) {
	entry["exempt_escalation_classes"] = []string{"spend"}
}

// Case (e): a plain spend request. The answer waits 10 minutes; until then the
// issue stays held and a pass delivers nothing.
func TestHeldIssueSpendAnswerWaitsForWakeAt(test *testing.T) {
	card := newHeldCard(test, "spend", "", exemptSpend)
	tasks := card.tasks(test)
	card.answer(test)
	var ahead bool
	dbfx.QueryRow(test, `SELECT wake_at > clock_timestamp() + interval '9 minutes' FROM decision_answer WHERE decision_request_id = $1`, card.decision.ID).Scan(&ahead)
	if !ahead {
		test.Fatal("the answer's wake_at is not about 10 minutes ahead")
	}
	wantHold(test, card.issueID, true, true, intakehold.ReasonHeld)
	before := card.deliveryRows(test)
	card.pass(test)
	if card.deliveryRows(test) != before {
		test.Fatalf("a pass before wake_at changed a row: %s -> %s", before, card.deliveryRows(test))
	}
	card.wantParked(test)

	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE decision_request_id = $1`, card.decision.ID)
	field := readHold(test, card.issueID)
	if field != nil && field["held"] == true {
		test.Fatalf("intake_hold = %v after wake_at, want not held", field)
	}
	card.pass(test)
	card.wantAnswerWake(test, "run_enqueued")
	if after := card.tasks(test); after != tasks+1 {
		test.Fatalf("tasks %d -> %d, want exactly one queued", tasks, after)
	}
}

// Case (e), withdrawal variant: the person withdraws before wake_at.
func TestHeldIssueSpendAnswerWithdrawnBeforeWakeAt(test *testing.T) {
	card := newHeldCard(test, "spend", "", exemptSpend)
	card.answer(test)
	var answerID string
	dbfx.QueryRow(test, `SELECT id::text FROM decision_answer WHERE decision_request_id = $1`, card.decision.ID).Scan(&answerID)
	tasks := card.tasks(test)
	request := card.request()
	if _, err := testHandler.disclaimDecisionAnswerTx(request, card.issue.WorkspaceID, parseUUID(answerID), parseUUID(card.humanID)); err != nil {
		test.Fatal(err)
	}
	wantHold(test, card.issueID, true, true, intakehold.ReasonHeld)
	card.pass(test)
	wantHold(test, card.issueID, true, true, intakehold.ReasonHeld)
	if after := card.tasks(test); after != tasks {
		test.Fatalf("tasks %d -> %d, want none queued", tasks, after)
	}
}

// Case (f): an action request has no platform delay, so the answer ends the
// hold and the receipts deliver.
func TestHeldIssueSpendActionAnswerDelivers(test *testing.T) {
	card := newHeldCard(test, "spend", "action", exemptSpend)
	card.answer(test)
	card.pass(test)
	found := card.receipts(test, "answered")
	if len(found) == 0 {
		test.Fatal("no answered receipt")
	}
	for _, row := range found {
		if row.Status != "delivered" || row.Outcome == "" {
			test.Fatalf("receipt = %+v, want delivered with an outcome", row)
		}
	}
	if queued := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'running')`, card.issueID); queued < 1 {
		test.Fatal("no task queued for the delivered action answer")
	}
	card.wantBackInBacklog(test)
	if field := readHold(test, card.issueID); field != nil && field["held"] == true {
		test.Fatalf("intake_hold = %v, want not held", field)
	}
}

// Case (h): mode warn dispatches as today and writes one intake_hold_warn line.
func TestHeldIssueWarnModeDispatchesAndLogs(test *testing.T) {
	card := newHeldCard(test, "reversible", "", func(entry map[string]any) { entry["mode"] = "warn" })
	logs := recordHoldLogs(test)
	tasks := card.tasks(test)
	card.answer(test)
	card.pass(test)
	card.wantAnswerWake(test, "run_enqueued")
	if after := card.tasks(test); after != tasks+1 {
		test.Fatalf("tasks %d -> %d, want exactly one queued", tasks, after)
	}
	logs.wantSite(test, "intake_hold_warn", "decision_wake", 1)
	if skipped := logs.lines("intake_hold_delivery_skipped"); len(skipped) != 0 {
		test.Fatalf("warn mode wrote skip lines: %v", skipped)
	}
}
