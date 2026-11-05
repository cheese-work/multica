package handler

import (
	"context"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"testing"
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
