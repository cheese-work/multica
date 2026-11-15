package handler

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// actionIDs is the batch's action ids in id order, the order the card lists them.
func (run batchRun) actionIDs(test *testing.T) []string {
	test.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT a.id::text FROM decision_action a JOIN decision_request r ON r.id = a.decision_request_id WHERE r.batch_id = $1 ORDER BY a.id`, run.batchID)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			test.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// choose answers every action of the batch: the given indexes reject, the rest approve.
func (run batchRun) choose(test *testing.T, rejected ...int) map[string]any {
	test.Helper()
	choices := map[string]any{}
	for index, id := range run.actionIDs(test) {
		choices[id] = "approve"
		for _, reject := range rejected {
			if reject == index {
				choices[id] = "reject"
			}
		}
	}
	return choices
}

// subsetHash is the hash of the approved items only.
func (run batchRun) subsetHash(test *testing.T, rejected ...int) string {
	test.Helper()
	var hashes []string
	for index, id := range run.actionIDs(test) {
		var hash string
		dbfx.QueryRow(test, `SELECT payload_sha256 FROM decision_action WHERE id = $1`, id).Scan(&hash)
		if !intIn(index, rejected) {
			hashes = append(hashes, hash)
		}
	}
	return batchPayloadSHA256(hashes)
}

func intIn(want int, values []int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (run batchRun) chosenClaim(test *testing.T, rejected ...int) map[string]any {
	return map[string]any{"payload_sha256": run.subsetHash(test, rejected...), "choices": run.choose(test, rejected...), "assertion": map[string]any{"id": "fake"}}
}

func TestBatchApproveWithMixedChoiceAppliesTwoAndRejectsOne(test *testing.T) {
	run := newBatchRun(test, []string{"che5695 mix a", "che5695 mix b", "che5695 mix c"}, nil)
	var signed string
	verifier := func(_ context.Context, _ *db.Queries, claim ApprovalClaim) (pgtype.UUID, error) {
		signed = claim.PayloadSHA256
		return pgtype.UUID{}, nil
	}
	var out batchOutcome
	decodeJSON(test, run.approve(test, withVerifier(verifier), run.chosenClaim(test, 2), http.StatusOK), &out)
	if len(out.Items) != 3 {
		test.Fatalf("outcome = %+v, want 3 items", out)
	}
	for index, want := range []string{"executed", "executed", "rejected"} {
		if out.Items[index].State != want {
			test.Fatalf("item %d is %s, want %s (outcome %+v)", index, out.Items[index].State, want, out)
		}
	}
	if copies := run.copies(test); copies != 2 {
		test.Fatalf("copies = %d, want 2", copies)
	}
	if want := run.subsetHash(test, 2); signed != want || signed == run.hash(test) {
		test.Fatalf("signed hash %s, want the approved subset's %s, not the whole batch's", signed, want)
	}
	if answered := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE batch_id = $1 AND status = 'answered'`, run.batchID); answered != 3 {
		test.Fatalf("answered requests = %d, want 3", answered)
	}
}

func TestBatchApproveSignedForTheWholeBatchIsRefusedWhenOneIsRejected(test *testing.T) {
	run := newBatchRun(test, []string{"che5695 whole a", "che5695 whole b"}, nil)
	claim := run.chosenClaim(test, 1)
	claim["payload_sha256"] = run.hash(test)
	body := run.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict)
	if !strings.Contains(body, "payload_mismatch") {
		test.Fatalf("body %q lacks payload_mismatch", body)
	}
	for _, name := range run.names {
		if state := run.state(test, name); state != "pending" {
			test.Fatalf("%s is %s, want pending: nothing applies on a wrong hash", name, state)
		}
	}
}

func TestBatchApproveRefusesAMissingOrUnknownChoice(test *testing.T) {
	run := newBatchRun(test, []string{"che5695 gap a", "che5695 gap b", "che5695 gap c"}, nil)
	ids := run.actionIDs(test)
	missing := run.chosenClaim(test)
	delete(missing["choices"].(map[string]any), ids[2])
	unknown := run.chosenClaim(test)
	unknown["choices"].(map[string]any)["00000000-0000-0000-0000-000000000000"] = "reject"
	badValue := run.chosenClaim(test)
	badValue["choices"].(map[string]any)[ids[0]] = "maybe"
	for name, claim := range map[string]map[string]any{"missing": missing, "unknown": unknown, "bad value": badValue} {
		if body := run.approve(test, withVerifier(acceptingVerifier), claim, http.StatusBadRequest); !strings.Contains(body, "choice_mismatch") {
			test.Fatalf("%s: body %q lacks choice_mismatch", name, body)
		}
	}
	for _, name := range run.names {
		if state := run.state(test, name); state != "pending" {
			test.Fatalf("%s is %s, want pending", name, state)
		}
	}
}

func TestBatchApproveOfOnlyRejectsNeedsNoPasskey(test *testing.T) {
	run := newBatchRun(test, []string{"che5695 none a", "che5695 none b"}, nil)
	var calls atomic.Int32
	verifier := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
		calls.Add(1)
		return pgtype.UUID{}, nil
	}
	var out batchOutcome
	decodeJSON(test, run.approve(test, withVerifier(verifier), map[string]any{"choices": run.choose(test, 0, 1)}, http.StatusOK), &out)
	for index, item := range out.Items {
		if item.State != "rejected" {
			test.Fatalf("item %d is %s, want rejected", index, item.State)
		}
	}
	if calls.Load() != 0 || run.copies(test) != 0 {
		test.Fatalf("verifier calls = %d, copies = %d, want 0 and 0", calls.Load(), run.copies(test))
	}
}

func TestBatchApproveReplayOfAMixedChoiceDoesNothingTwice(test *testing.T) {
	run := newBatchRun(test, []string{"che5695 replay a", "che5695 replay b", "che5695 replay c"}, nil)
	claim := run.chosenClaim(test, 2)
	run.approve(test, withVerifier(acceptingVerifier), claim, http.StatusOK)
	if body := run.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict); !strings.Contains(body, "not_pending") {
		test.Fatalf("body %q lacks not_pending", body)
	}
	if copies := run.copies(test); copies != 2 {
		test.Fatalf("copies = %d, want 2", copies)
	}
	if answers := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id IN (SELECT id FROM decision_request WHERE batch_id = $1)`, run.batchID); answers != 3 {
		test.Fatalf("answers = %d, want 3: one per item, none repeated", answers)
	}
}

func TestBatchApproveWithEveryItemChosenApproveSignsTheSameHashAsNoChoices(test *testing.T) {
	run := newBatchRun(test, []string{"che5695 same a", "che5695 same b"}, nil)
	if run.subsetHash(test) != run.hash(test) {
		test.Fatal("all-approve subset hash differs from the whole-batch hash")
	}
	claim := run.claim(test)
	claim["choices"] = run.choose(test)
	run.approve(test, withVerifier(acceptingVerifier), claim, http.StatusOK)
	if copies := run.copies(test); copies != 2 {
		test.Fatalf("copies = %d, want 2", copies)
	}
}

func TestBatchCardReadCarriesTheBatchIDAndItsItems(test *testing.T) {
	run := newBatchRun(test, []string{"che5695 card a", "che5695 card b", "che5695 card c"}, nil)
	var pingID string
	dbfx.QueryRow(test, `SELECT ping_message_id::text FROM decision_request WHERE batch_id = $1 LIMIT 1`, run.batchID).Scan(&pingID)
	read := func() []DecisionRequestCard {
		return testHandler.decisionRequestsForMessages(context.Background(), testWorkspaceID, []db.ChatMessage{{ID: parseUUID(pingID)}})[pingID]
	}
	check := func(want []string) {
		test.Helper()
		cards := read()
		if len(cards) != 3 {
			test.Fatalf("cards = %d, want 3", len(cards))
		}
		ids := run.actionIDs(test)
		for _, card := range cards {
			if card.BatchID != run.batchID || len(card.BatchItems) != 3 {
				test.Fatalf("card batch_id %q items %d, want %s and 3", card.BatchID, len(card.BatchItems), run.batchID)
			}
			for index, item := range card.BatchItems {
				if item.ActionID != ids[index] || item.IssueID == "" || item.IssueIdentifier == "" || item.State != want[index] {
					test.Fatalf("item %d = %+v, want action %s state %s", index, item, ids[index], want[index])
				}
			}
		}
	}
	check([]string{"pending", "pending", "pending"})
	run.approve(test, withVerifier(acceptingVerifier), run.chosenClaim(test, 2), http.StatusOK)
	check([]string{"executed", "executed", "rejected"})
	if result := read()[0].BatchItems[0].Result; len(result) == 0 {
		test.Fatal("executed item carries no result")
	}
}
