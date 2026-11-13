package handler

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-2394 P2a: durable wake event and receipt tables, the per-request
// wake_seq counter, and the legacy wake query ignoring kind=action.

func TestDecisionWakeTablesEnforceDesignKeys(test *testing.T) {
	for _, key := range []struct{ table, columns string }{
		{"decision_wake_event", "{request_id,seq}"},
		{"decision_wake_receipt", "{event_id,recipient_agent_id}"},
	} {
		if n := dbfx.Count(test, `SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid
			WHERE c.relname = $1 AND i.indisunique AND (
				SELECT array_agg(a.attname::text ORDER BY k.ord) FROM unnest(i.indkey) WITH ORDINALITY k(attnum, ord)
				JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
			) = $2::text[]`, key.table, key.columns); n != 1 {
			test.Fatalf("%s: unique key on %s count = %d, want 1", key.table, key.columns, n)
		}
	}
}

func TestDecisionWakeSeqIsDistinctAndIncreasing(test *testing.T) {
	fixture, _ := readyDecisionDelivery(test)
	request := fixture.decision.ID
	var first, second int64
	for _, seq := range []*int64{&first, &second} {
		dbfx.QueryRow(test, `UPDATE decision_request SET wake_seq = wake_seq + 1 WHERE id = $1 RETURNING wake_seq`, request).Scan(seq)
		dbfx.Exec(test, `INSERT INTO decision_wake_event (id, request_id, seq, type) VALUES (gen_random_uuid(), $1, $2, 'answered')`, request, *seq)
	}
	if first != 1 || second != 2 {
		test.Fatalf("wake_seq = %d then %d, want 1 then 2", first, second)
	}
	if n := dbfx.Count(test, `SELECT count(DISTINCT seq) FROM decision_wake_event WHERE request_id = $1`, request); n != 2 {
		test.Fatalf("distinct event seq = %d, want 2", n)
	}
	// Same (request, seq) twice is a key collision.
	if _, err := testPool.Exec(context.Background(), `INSERT INTO decision_wake_event (id, request_id, seq, type) VALUES (gen_random_uuid(), $1, 1, 'answered')`, request); err == nil {
		test.Fatal("duplicate (request_id, seq) was accepted")
	}
	// One receipt per (event, recipient).
	var event string
	dbfx.QueryRow(test, `SELECT id FROM decision_wake_event WHERE request_id = $1 AND seq = 1`, request).Scan(&event)
	insert := `INSERT INTO decision_wake_receipt (id, event_id, event_seq, recipient_agent_id, role) VALUES (gen_random_uuid(), $1, 1, $2, 'requester')`
	dbfx.Exec(test, insert, event, fixture.agentID)
	if _, err := testPool.Exec(context.Background(), insert, event, fixture.agentID); err == nil {
		test.Fatal("duplicate (event_id, recipient_agent_id) was accepted")
	}
}

func TestLegacyWakeCandidatesSkipActionRequests(test *testing.T) {
	decision, _ := readyDecisionDelivery(test)
	action, _ := readyDecisionDelivery(test)
	dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, action.decision.ID)
	candidates, err := testHandler.Queries.ListDecisionWakeCandidates(context.Background(), 100)
	if err != nil {
		test.Fatal(err)
	}
	var sawDecision, sawAction bool
	for _, candidate := range candidates {
		sawDecision = sawDecision || candidate.ID == decision.decision.ID
		sawAction = sawAction || candidate.ID == action.decision.ID
	}
	if !sawDecision || sawAction {
		test.Fatalf("kind=decision listed=%v (want true), kind=action listed=%v (want false)", sawDecision, sawAction)
	}
}

// The sqlc queries P2b will call: allocate under the request, then insert the
// event and receipt with that value.
func TestDecisionWakeQueriesAllocateAndInsert(test *testing.T) {
	fixture, _ := readyDecisionDelivery(test)
	ctx := context.Background()
	queries := testHandler.Queries
	var seqs []int64
	for range 2 {
		seq, err := queries.AllocateDecisionWakeSeq(ctx, fixture.decision.ID)
		if err != nil {
			test.Fatal(err)
		}
		event, err := queries.InsertDecisionWakeEvent(ctx, db.InsertDecisionWakeEventParams{ID: dbid.NewV7(), RequestID: fixture.decision.ID, Seq: seq, Type: "answered"})
		if err != nil {
			test.Fatal(err)
		}
		if _, err := queries.InsertDecisionWakeReceipt(ctx, db.InsertDecisionWakeReceiptParams{ID: dbid.NewV7(), EventID: event.ID, EventSeq: seq, RecipientAgentID: fixture.decision.AskingAgentID, Role: "requester"}); err != nil {
			test.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	if seqs[0] >= seqs[1] {
		test.Fatalf("wake_seq not increasing: %v", seqs)
	}
}

// The tables have no foreign key, so the decision worker's orphan cleanup
// deletes wake events and receipts with the request they belong to.
func TestDecisionWakeRowsLeaveWithOrphanedRequest(test *testing.T) {
	fixture, _ := readyDecisionDelivery(test)
	dbfx.Exec(test, `INSERT INTO decision_wake_event (id, request_id, seq, type) VALUES (gen_random_uuid(), $1, 1, 'answered')`, fixture.decision.ID)
	dbfx.Exec(test, `INSERT INTO decision_wake_receipt (id, event_id, event_seq, recipient_agent_id, role)
		SELECT gen_random_uuid(), id, 1, $2, 'requester' FROM decision_wake_event WHERE request_id = $1`, fixture.decision.ID, fixture.agentID)
	test.Cleanup(func() {
		dbfx.Exec(test, `DELETE FROM decision_wake_receipt WHERE event_id IN (SELECT id FROM decision_wake_event WHERE request_id = $1)`, fixture.decision.ID)
		dbfx.Exec(test, `DELETE FROM decision_wake_event WHERE request_id = $1`, fixture.decision.ID)
	})
	dbfx.Exec(test, `DELETE FROM issue WHERE id = $1`, fixture.issue.ID)
	if err := testHandler.cleanupDecisionOrphans(context.Background(), fixture.issue.WorkspaceID, fixture.issue.ID); err != nil {
		test.Fatal(err)
	}
	events := dbfx.Count(test, `SELECT count(*) FROM decision_wake_event WHERE request_id = $1`, fixture.decision.ID)
	receipts := dbfx.Count(test, `SELECT count(*) FROM decision_wake_receipt WHERE recipient_agent_id = $1 AND event_seq = 1`, fixture.agentID)
	if events != 0 || receipts != 0 {
		test.Fatalf("orphan cleanup left %d events and %d receipts", events, receipts)
	}
}

// CHE-2394 P2b: the shared mark-answered path writes the answered event and
// one receipt per distinct recipient for kind=action only.

type wakeReceiptRow struct {
	Role    string
	Agent   string
	SquadID pgtype.UUID
	Seq     int64
}

func wakeReceipts(test *testing.T, request pgtype.UUID) []wakeReceiptRow {
	test.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT r.role, r.recipient_agent_id::text, r.squad_id, r.event_seq
		FROM decision_wake_receipt r JOIN decision_wake_event e ON e.id = r.event_id WHERE e.request_id = $1 ORDER BY r.role`, request)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	var out []wakeReceiptRow
	for rows.Next() {
		var row wakeReceiptRow
		if err := rows.Scan(&row.Role, &row.Agent, &row.SquadID, &row.Seq); err != nil {
			test.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

// answeredActionFixture records an answer on a kind=action request.
func answeredActionFixture(test *testing.T) (askedRecordFixture, decisionRecordResult) {
	test.Helper()
	fixture := newAskedRecordFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, fixture.decision.ID)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	return fixture, recorded
}

func TestAnsweredActionOnSquadIssueWritesEventAndTwoReceipts(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	leader := dbfx.Agent(test, "wake receipt leader", handlerTestRuntimeID(test))
	squadID := dbfx.Squad(test, "Wake receipts", leader)
	dbfx.Exec(test, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
	dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, fixture.decision.ID)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	var eventAnswer pgtype.UUID
	var seq int64
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_wake_event WHERE request_id = $1 AND type = 'answered'`, fixture.decision.ID); n != 1 {
		test.Fatalf("answered events = %d, want 1", n)
	}
	dbfx.QueryRow(test, `SELECT answer_id, seq FROM decision_wake_event WHERE request_id = $1`, fixture.decision.ID).Scan(&eventAnswer, &seq)
	if eventAnswer != recorded.Answer.ID || seq != 1 {
		test.Fatalf("event answer = %v seq = %d, want %v and 1", eventAnswer, seq, recorded.Answer.ID)
	}
	receipts := wakeReceipts(test, fixture.decision.ID)
	if len(receipts) != 2 {
		test.Fatalf("receipts = %+v, want 2", receipts)
	}
	assignee, requester := receipts[0], receipts[1] // ordered by role
	if assignee.Role != "assignee" || assignee.Agent != leader || uuidToString(assignee.SquadID) != squadID || assignee.Seq != 1 {
		test.Fatalf("assignee receipt = %+v, want leader %s with squad %s", assignee, leader, squadID)
	}
	if requester.Role != "requester" || requester.Agent != fixture.agentID || requester.SquadID.Valid {
		test.Fatalf("requester receipt = %+v, want agent %s without squad", requester, fixture.agentID)
	}
}

func TestAnsweredActionWithSameRequesterAndAssigneeWritesOneReceipt(test *testing.T) {
	fixture, _ := answeredActionFixture(test) // fixture issue is assigned to the asking agent
	receipts := wakeReceipts(test, fixture.decision.ID)
	if len(receipts) != 1 || receipts[0].Role != "requester" || receipts[0].Agent != fixture.agentID {
		test.Fatalf("receipts = %+v, want one requester receipt for %s", receipts, fixture.agentID)
	}
}

func TestTwoAnswersOnOneActionRequestWriteTwoEventsWithoutKeyCollision(test *testing.T) {
	fixture, recorded := answeredActionFixture(test)
	ctx := context.Background()
	second := dbid.NewV7()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer tx.Rollback(ctx)
	request, err := testHandler.Queries.WithTx(tx).GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.decision.ID})
	if err != nil {
		test.Fatal(err)
	}
	if err := testHandler.recordDecisionWakeEvent(ctx, testHandler.Queries.WithTx(tx), request, "answered", second, pgtype.UUID{}); err != nil {
		test.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if n := dbfx.Count(test, `SELECT count(DISTINCT seq) FROM decision_wake_event WHERE request_id = $1 AND answer_id IN ($2, $3)`, fixture.decision.ID, recorded.Answer.ID, second); n != 2 {
		test.Fatalf("distinct events = %d, want 2", n)
	}
	if got := wakeReceipts(test, fixture.decision.ID); len(got) != 2 {
		test.Fatalf("receipts = %+v, want one per event", got)
	}
}

func TestAnsweredDecisionRequestWritesNoEventAndKeepsLegacyWake(test *testing.T) {
	fixture, _ := readyDecisionDelivery(test)
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_wake_event WHERE request_id = $1`, fixture.decision.ID); n != 0 {
		test.Fatalf("kind=decision wrote %d events", n)
	}
	candidates, err := testHandler.Queries.ListDecisionWakeCandidates(context.Background(), 100)
	if err != nil {
		test.Fatal(err)
	}
	if !slices.ContainsFunc(candidates, func(c db.DecisionRequest) bool { return c.ID == fixture.decision.ID }) {
		test.Fatal("kind=decision lost its legacy wake")
	}
}

// A need_decision issue belongs to its assignee: an answered action card
// writes one receipt, for the assignee, and no run goes to the requester.
func TestAnsweredActionOnNeedDecisionIssueWakesOnlyTheAssignee(test *testing.T) {
	runner := newDeliveryRunner(test)
	fixture := newAskedRecordFixture(test)
	assignee := createHandlerTestAgent(test, "need decision assignee", []byte("[]"))
	dbfx.Exec(test, `UPDATE issue SET status = 'need_decision', assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, assignee, fixture.issueID)
	dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, fixture.decision.ID)
	if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); err != nil {
		test.Fatal(err)
	}
	receipts := wakeReceipts(test, fixture.decision.ID)
	if len(receipts) != 1 || receipts[0].Role != "assignee" || receipts[0].Agent != assignee {
		test.Fatalf("receipts = %+v, want one assignee receipt for %s", receipts, assignee)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	finalizeAnswerWake(test, fixture)
	decisionPass(test, runner)
	decisionPass(test, runner)
	if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched', 'running')`, fixture.issueID, assignee); n != 1 {
		test.Fatalf("assignee runs = %d, want 1", n)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched', 'running')`, fixture.issueID, fixture.agentID); n != 0 {
		test.Fatalf("requester runs = %d, want 0", n)
	}
}

// Negative control: an assignee the person cannot invoke gets no receipt; the
// requester keeps the wake.
func TestAnsweredActionOnNeedDecisionIssueSkipsAnAssigneeThePersonCannotInvoke(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	assignee := dbfx.Agent(test, "restricted assignee", handlerTestRuntimeID(test))
	dbfx.Exec(test, `UPDATE agent SET permission_mode = 'private', owner_id = NULL WHERE id = $1`, assignee)
	dbfx.Exec(test, `UPDATE issue SET status = 'need_decision', assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, assignee, fixture.issueID)
	dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, fixture.decision.ID)
	if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); err != nil {
		test.Fatal(err)
	}
	receipts := wakeReceipts(test, fixture.decision.ID)
	if len(receipts) != 1 || receipts[0].Role != "requester" || receipts[0].Agent != fixture.agentID {
		test.Fatalf("receipts = %+v, want one requester receipt", receipts)
	}
}

// Negative control: permission revoked after the receipt was written but
// before it dispatches. The receipt settles without a run.
func TestAnsweredActionReceiptDoesNotDispatchAfterTheAssigneeInvokeIsRevoked(test *testing.T) {
	runner := newDeliveryRunner(test)
	fixture := newAskedRecordFixture(test)
	assignee := createHandlerTestAgent(test, "revoked assignee", []byte("[]"))
	dbfx.Exec(test, `UPDATE issue SET status = 'need_decision', assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, assignee, fixture.issueID)
	dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, fixture.decision.ID)
	if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); err != nil {
		test.Fatal(err)
	}
	receipts := wakeReceipts(test, fixture.decision.ID)
	if len(receipts) != 1 || receipts[0].Role != "assignee" {
		test.Fatalf("receipts = %+v, want one assignee receipt", receipts)
	}
	dbfx.Exec(test, `UPDATE agent SET permission_mode = 'private', owner_id = $2 WHERE id = $1`, assignee, createEphemeralUser(test, "revoker"))
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	finalizeAnswerWake(test, fixture)
	decisionPass(test, runner)
	decisionPass(test, runner)
	if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, fixture.issueID, assignee); n != 0 {
		test.Fatalf("revoked assignee runs = %d, want 0", n)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_wake_receipt WHERE event_id IN (SELECT id FROM decision_wake_event WHERE request_id = $1) AND status = 'skipped_ineligible'`, fixture.decision.ID); n != 1 {
		test.Fatalf("skipped receipts = %d, want 1", n)
	}
}
