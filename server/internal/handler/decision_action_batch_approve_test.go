package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// batchRun is one open batch card of agent.copy items, each on its own issue.
type batchRun struct {
	dmProposalFixture
	batchID string
	names   []string
}

// newBatchRun proposes one copy per name and readies the human to approve it.
// source returns the agent a copy item copies; nil means the fixture's agent.
func newBatchRun(test *testing.T, names []string, source func(fixture dmProposalFixture, index int) string) batchRun {
	test.Helper()
	fixture := newDMProposalFixture(test)
	dbfx.Exec(test, `UPDATE agent SET instructions = 'be careful' WHERE id = $1`, fixture.agentID)
	var runtimeID, was string
	dbfx.QueryRow(test, `SELECT runtime_id::text FROM agent WHERE id = $1`, fixture.agentID).Scan(&runtimeID)
	dbfx.QueryRow(test, `SELECT visibility FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&was)
	dbfx.Exec(test, `UPDATE agent_runtime SET visibility = 'public' WHERE id = $1`, runtimeID)
	dbfx.Cleanup(test, `UPDATE agent_runtime SET visibility = '`+was+`' WHERE id = '`+runtimeID+`'`)
	items := make([]map[string]any, len(names))
	for index, name := range names {
		items[index] = fixture.copyItem(test, "batch approve "+name)
		params := items[index]["params"].(map[string]any)
		params["name"] = name
		if source != nil {
			if id := source(fixture, index); id != "" {
				params["source_agent_id"] = id
			}
		}
		dbfx.Cleanup(test, `DELETE FROM agent WHERE name = $1`, name)
	}
	view := fixture.batchPropose(test, fixture.batchBody(items...), http.StatusCreated)
	run := batchRun{dmProposalFixture: fixture, batchID: view.Batches[0].BatchID, names: names}
	for index, request := range view.Batches[0].Requests {
		dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, request.ID)
		placeDecisionHuman(test, items[index]["issue_id"].(string), fixture.humanID, "member", true)
	}
	return run
}

// hash is the batch payload hash the human approves: the hash of the sorted
// per-item payload hashes.
func (run batchRun) hash(test *testing.T) string {
	test.Helper()
	var hashes []string
	dbfx.QueryRow(test, `SELECT array_agg(a.payload_sha256) FROM decision_action a JOIN decision_request r ON r.id = a.decision_request_id WHERE r.batch_id = $1`, run.batchID).Scan(&hashes)
	return batchPayloadSHA256(hashes)
}

func (run batchRun) claim(test *testing.T) map[string]any {
	return map[string]any{"payload_sha256": run.hash(test), "assertion": map[string]any{"id": "fake"}}
}

func (run batchRun) approve(test *testing.T, handler *Handler, body map[string]any, status int) string {
	test.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		test.Fatal(err)
	}
	request := withURLParam(newRequest(http.MethodPost, "/api/decision-actions/batches/"+run.batchID+"/approve", nil), "id", run.batchID)
	request.Header.Set("X-User-ID", run.humanID)
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	request.Body = readerCloser{bytes.NewReader(raw)}
	return testutil.Call(test, handler.ApproveDecisionActionBatch, request).Want(status).Text()
}

type batchOutcome struct {
	BatchID string `json:"batch_id"`
	Items   []struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		ErrorCode string `json:"error_code"`
	} `json:"items"`
}

func (run batchRun) state(test *testing.T, name string) string {
	test.Helper()
	var state string
	dbfx.QueryRow(test, `SELECT a.state FROM decision_action a JOIN decision_request r ON r.id = a.decision_request_id WHERE r.batch_id = $1 AND a.params->>'name' = $2`, run.batchID, name).Scan(&state)
	return state
}

func (run batchRun) copies(test *testing.T) int {
	test.Helper()
	count := 0
	for _, name := range run.names {
		count += dbfx.Count(test, `SELECT count(*) FROM agent WHERE name = $1 AND archived_at IS NULL`, name)
	}
	return count
}

func TestBatchApproveAppliesEveryItem(test *testing.T) {
	run := newBatchRun(test, []string{"che5413 all a", "che5413 all b", "che5413 all c"}, nil)
	var out batchOutcome
	decodeJSON(test, run.approve(test, withVerifier(acceptingVerifier), run.claim(test), http.StatusOK), &out)
	if out.BatchID != run.batchID || len(out.Items) != 3 {
		test.Fatalf("outcome = %+v, want batch %s with 3 items", out, run.batchID)
	}
	for _, item := range out.Items {
		if item.State != "executed" {
			test.Fatalf("item %+v, want executed", item)
		}
	}
	if copies := run.copies(test); copies != 3 {
		test.Fatalf("copies = %d, want 3", copies)
	}
	if answered := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE batch_id = $1 AND status = 'answered'`, run.batchID); answered != 3 {
		test.Fatalf("answered requests = %d, want 3", answered)
	}
}

func TestBatchApproveFailsOnlyTheItemThatDrifted(test *testing.T) {
	var drifting string
	names := []string{"che5413 drift a", "che5413 drift b", "che5413 drift c"}
	run := newBatchRun(test, names, func(fixture dmProposalFixture, index int) string {
		if index != 1 {
			return ""
		}
		dbfx.Exec(test, `INSERT INTO agent (workspace_id, name, runtime_mode, runtime_id, owner_id, instructions) SELECT workspace_id, 'che5413 drift source', runtime_mode, runtime_id, owner_id, 'be careful' FROM agent WHERE id = $1`, fixture.agentID)
		dbfx.Cleanup(test, `DELETE FROM agent WHERE name = 'che5413 drift source'`)
		dbfx.QueryRow(test, `SELECT id::text FROM agent WHERE name = 'che5413 drift source'`).Scan(&drifting)
		return drifting
	})
	dbfx.Exec(test, `UPDATE agent SET instructions = 'changed after the ask' WHERE id = $1`, drifting)
	var out batchOutcome
	decodeJSON(test, run.approve(test, withVerifier(acceptingVerifier), run.claim(test), http.StatusOK), &out)
	codes := map[string]string{}
	for _, item := range out.Items {
		codes[item.State] += item.ErrorCode
	}
	if len(out.Items) != 3 || codes["executed"] != "" || codes["failed"] != "precondition_drift" {
		test.Fatalf("outcome = %+v, want 2 executed and 1 failed precondition_drift", out)
	}
	if run.state(test, names[0]) != "executed" || run.state(test, names[1]) != "failed" || run.state(test, names[2]) != "executed" {
		test.Fatal("states are not executed/failed/executed")
	}
	if copies := run.copies(test); copies != 2 {
		test.Fatalf("copies = %d, want 2", copies)
	}
	if answered := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE batch_id = $1 AND status = 'answered'`, run.batchID); answered != 3 {
		test.Fatalf("answered requests = %d, want 3: every item ends with a result", answered)
	}
}

func TestBatchApproveRollsBackAnItemWhoseExecutionFails(test *testing.T) {
	names := []string{"che5413 taken a", "che5413 taken b"}
	run := newBatchRun(test, names, nil)
	dbfx.Exec(test, `INSERT INTO agent (workspace_id, name, runtime_mode, runtime_id, owner_id) SELECT workspace_id, $2, runtime_mode, runtime_id, owner_id FROM agent WHERE id = $1`, run.agentID, names[0])
	var out batchOutcome
	decodeJSON(test, run.approve(test, withVerifier(acceptingVerifier), run.claim(test), http.StatusOK), &out)
	if run.state(test, names[0]) != "failed" || run.state(test, names[1]) != "executed" {
		test.Fatalf("outcome = %+v, want the taken name failed and the other executed", out)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_action a JOIN decision_request r ON r.id = a.decision_request_id WHERE r.batch_id = $1 AND a.error_code = 'name_taken'`, run.batchID); n != 1 {
		test.Fatal("failure is not recorded as name_taken")
	}
}

func TestBatchApproveWithAWrongHashAppliesNothing(test *testing.T) {
	run := newBatchRun(test, []string{"che5413 hash a", "che5413 hash b", "che5413 hash c"}, nil)
	claim := run.claim(test)
	claim["payload_sha256"] = strings.Repeat("0", 64)
	body := run.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict)
	if !strings.Contains(body, "payload_mismatch") {
		test.Fatalf("body %q lacks payload_mismatch", body)
	}
	for _, name := range run.names {
		if state := run.state(test, name); state != "pending" {
			test.Fatalf("%s is %s, want pending", name, state)
		}
	}
	if copies := run.copies(test); copies != 0 {
		test.Fatalf("copies = %d, want 0", copies)
	}
}

func TestBatchApproveTwiceIsRefusedAsNotPending(test *testing.T) {
	run := newBatchRun(test, []string{"che5413 twice a", "che5413 twice b"}, nil)
	run.approve(test, withVerifier(acceptingVerifier), run.claim(test), http.StatusOK)
	body := run.approve(test, withVerifier(acceptingVerifier), run.claim(test), http.StatusConflict)
	if !strings.Contains(body, "not_pending") {
		test.Fatalf("body %q lacks not_pending", body)
	}
	if copies := run.copies(test); copies != 2 {
		test.Fatalf("copies = %d, want 2", copies)
	}
}
