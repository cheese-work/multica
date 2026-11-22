package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5491 (S2b): a status move that carries an operation_key writes a receipt
// in its own transaction, for every handler type. A retry of the same request
// returns the first result; the same key with another body or actor is refused.

type opFixture struct {
	issueID string
	agentID string // the agent handler; empty for member and none
}

// opKinds are the three handler types the receipt must cover.
var opKinds = []string{"agent", "member", "none"}

func newOpFixture(t *testing.T, kind string) opFixture {
	t.Helper()
	if kind == "agent" {
		issueID, agentID := handoffDispatchFixture(t)
		t.Cleanup(func() { cleanupOperations(issueID) })
		return opFixture{issueID: issueID, agentID: agentID}
	}
	seedTestCatalog(t)
	projectID := createWorkflowTestProject(t, "Operation receipt "+kind)
	column := map[string]any{"instructions": "Look at it."}
	if kind == "member" {
		column["handler"] = map[string]any{"type": "member", "id": testUserID}
	}
	setColumnHandoff(t, projectID, "in_progress", column).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "operation receipt "+kind, testutil.Cols{"project_id": projectID, "status": "todo"})
	t.Cleanup(func() { cleanupOperations(issueID) })
	return opFixture{issueID: issueID}
}

func cleanupOperations(issueID string) {
	testPool.Exec(context.Background(), `DELETE FROM issue_handoff_operation WHERE issue_id = $1`, issueID)
}

func (f opFixture) move(t *testing.T, body map[string]any, headers map[string]string) *testutil.Response {
	t.Helper()
	all := map[string]string{"X-Actor-Source": "interactive_jwt"}
	for k, v := range headers {
		all[k] = v
	}
	full := map[string]any{"status": "in_progress"}
	for k, v := range body {
		full[k] = v
	}
	return issueWriters[0].write(t, f.issueID, full, all)
}

func (f opFixture) counts(t *testing.T) (comments, history, runs int) {
	t.Helper()
	comments = dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, f.issueID)
	history = dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, f.issueID)
	if f.agentID != "" {
		runs = dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, f.issueID, f.agentID)
	}
	return
}

func (f opFixture) wantCounts(t *testing.T, comments, history, runs int) {
	t.Helper()
	c, h, r := f.counts(t)
	if c != comments || h != history || r != runs {
		t.Fatalf("comments/history/runs = %d/%d/%d, want %d/%d/%d", c, h, r, comments, history, runs)
	}
}

func commentBody(key, content string) map[string]any {
	return map[string]any{"operation_key": key, "handoff_comment": map[string]any{"content": content}}
}

func wantOpError(t *testing.T, resp *testutil.Response, status int, code string) {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	json.Unmarshal(resp.Body.Bytes(), &body)
	if resp.Code != status || body.Code != code {
		t.Fatalf("response = %d %s, want %d %s", resp.Code, resp.Body.String(), status, code)
	}
}

// Acceptance 1: a retry after a lost response returns the first result. One
// comment, one status-history row and, for an agent handler, one run.
func TestHandoffOperation_RetryReturnsFirstResult(t *testing.T) {
	for _, kind := range opKinds {
		t.Run(kind, func(t *testing.T) {
			f := newOpFixture(t, kind)
			key := uuid.NewString()
			body := commentBody(key, "please look at the migration")

			first := f.move(t, body, nil).Want(http.StatusOK)
			retry := f.move(t, body, nil).Want(http.StatusOK)

			runs := 0
			if kind == "agent" {
				runs = 1
			}
			f.wantCounts(t, 1, 1, runs)
			if !reflect.DeepEqual(first.Map(), retry.Map()) {
				t.Fatalf("retry body differs from the first result:\nfirst: %s\nretry: %s", first.Body.String(), retry.Body.String())
			}
			var dispatchSet bool
			var rows int
			dbfx.QueryRow(t, `SELECT count(*), COALESCE(bool_and(dispatch_id IS NOT NULL), false)
				FROM issue_handoff_operation WHERE issue_id = $1 AND operation_key = $2`, f.issueID, key).Scan(&rows, &dispatchSet)
			if rows != 1 || dispatchSet != (kind == "agent") {
				t.Fatalf("receipts = %d, dispatch_id set = %v; want 1 receipt, dispatch_id set only for an agent handler", rows, dispatchSet)
			}
		})
	}
}

// Acceptance 2: the same key with a changed body is refused and writes nothing.
func TestHandoffOperation_ChangedBodyRefused(t *testing.T) {
	for _, kind := range opKinds {
		t.Run(kind, func(t *testing.T) {
			f := newOpFixture(t, kind)
			key := uuid.NewString()
			f.move(t, commentBody(key, "first text"), nil).Want(http.StatusOK)
			c, h, r := f.counts(t)

			wantOpError(t, f.move(t, commentBody(key, "second text"), nil), http.StatusConflict, "operation_key_reused")
			f.wantCounts(t, c, h, r)
		})
	}
}

// Acceptance 3: the same key from another actor is refused.
func TestHandoffOperation_OtherActorRefused(t *testing.T) {
	for _, kind := range opKinds {
		t.Run(kind, func(t *testing.T) {
			f := newOpFixture(t, kind)
			other := createHandlerTestAgent(t, "che5491-other-"+kind, nil)
			key := uuid.NewString()
			body := commentBody(key, "same text")
			f.move(t, body, nil).Want(http.StatusOK)
			c, h, r := f.counts(t)

			resp := f.move(t, body, map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": other, "X-Task-ID": uuid.NewString()})
			wantOpError(t, resp, http.StatusConflict, "operation_key_reused")
			f.wantCounts(t, c, h, r)
		})
	}
}

// Acceptance 4: a move without a key behaves as today: no receipt, and a
// repeated request writes again.
func TestHandoffOperation_NoKeyWritesNoReceipt(t *testing.T) {
	for _, kind := range opKinds {
		t.Run(kind, func(t *testing.T) {
			f := newOpFixture(t, kind)
			body := map[string]any{"handoff_comment": map[string]any{"content": "no key"}}

			f.move(t, body, nil).Want(http.StatusOK)
			dbfx.Exec(t, `UPDATE issue SET status = 'todo' WHERE id = $1`, f.issueID)
			f.move(t, body, nil).Want(http.StatusOK)

			if n := dbfx.Count(t, `SELECT count(*) FROM issue_handoff_operation WHERE issue_id = $1`, f.issueID); n != 0 {
				t.Fatalf("receipts = %d, want 0 for a move without a key", n)
			}
			if c, _, _ := f.counts(t); c != 2 {
				t.Fatalf("comments = %d, want 2: without a key each request is its own move", c)
			}
		})
	}
}

// Acceptance 5 (Terra note 1): a receipt older than 7 days is pruned by the
// sweep. A retry with that key afterwards is a new move: it writes again.
func TestHandoffOperation_ExpiredReceiptPrunedRetryIsNewMove(t *testing.T) {
	for _, kind := range opKinds {
		t.Run(kind, func(t *testing.T) {
			f := newOpFixture(t, kind)
			fresh, old := uuid.NewString(), uuid.NewString()
			f.move(t, commentBody(old, "old move"), nil).Want(http.StatusOK)
			dbfx.Exec(t, `UPDATE issue SET status = 'todo' WHERE id = $1`, f.issueID)
			f.move(t, commentBody(fresh, "fresh move"), nil).Want(http.StatusOK)
			dbfx.Exec(t, `UPDATE issue_handoff_operation SET created_at = now() - interval '8 days' WHERE issue_id = $1 AND operation_key = $2`, f.issueID, old)
			dbfx.Exec(t, `UPDATE issue_handoff_operation SET created_at = now() - interval '6 days' WHERE issue_id = $1 AND operation_key = $2`, f.issueID, fresh)

			testHandler.SweepHandoffIntents(context.Background(), time.Now())

			keys := map[string]bool{}
			rows, err := testPool.Query(context.Background(), `SELECT operation_key FROM issue_handoff_operation WHERE issue_id = $1`, f.issueID)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var k string
				rows.Scan(&k)
				keys[k] = true
			}
			rows.Close()
			if keys[old] || !keys[fresh] {
				t.Fatalf("receipts after the sweep = %v, want the 8-day row gone and the 6-day row kept", keys)
			}

			// The issue is back where the first move found it, so the retry can move.
			dbfx.Exec(t, `UPDATE issue SET status = 'todo' WHERE id = $1`, f.issueID)
			commentsBefore, _, _ := f.counts(t)
			f.move(t, commentBody(old, "old move"), nil).Want(http.StatusOK)
			if c, _, _ := f.counts(t); c != commentsBefore+1 {
				t.Fatalf("comments = %d, want %d: a retry after the prune is a new move", c, commentsBefore+1)
			}
			if dbfx.Count(t, `SELECT count(*) FROM issue_handoff_operation WHERE issue_id = $1 AND operation_key = $2`, f.issueID, old) != 1 {
				t.Fatal("the new move did not write a fresh receipt")
			}
		})
	}
}

// failPostCommitStore makes the update that stores the full response after the
// commit fail for one issue: only the in-transaction write (which sets
// status_history_id) may change the receipt. It stands in for a process that
// stops, or a store that fails, between the commit and the response.
func failPostCommitStore(t *testing.T, issueID string) {
	t.Helper()
	fn := "che5491_fail_store_" + issueID[:8]
	trg := fn + "_trg"
	dbfx.Exec(t, `CREATE OR REPLACE FUNCTION `+fn+`() RETURNS trigger AS $$
		BEGIN
			IF OLD.issue_id = '`+issueID+`' AND NEW.status_history_id IS NOT DISTINCT FROM OLD.status_history_id THEN
				RAISE EXCEPTION 'simulated lost response store';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql`)
	dbfx.Exec(t, `CREATE TRIGGER `+trg+` BEFORE UPDATE ON issue_handoff_operation FOR EACH ROW EXECUTE FUNCTION `+fn+`()`)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+trg+` ON issue_handoff_operation`)
		testPool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)
	})
}

// Review finding P1 (failure-path): the first result must not depend on a write
// after the commit. With the post-commit store lost and the issue changed since,
// a retry still returns the first result.
func TestHandoffOperation_RetryAfterLostStoreAndLaterChangeReturnsFirstResult(t *testing.T) {
	for _, kind := range opKinds {
		t.Run(kind, func(t *testing.T) {
			f := newOpFixture(t, kind)
			failPostCommitStore(t, f.issueID)
			key := uuid.NewString()
			body := commentBody(key, "window move")

			first := f.move(t, body, nil).Want(http.StatusOK)
			dbfx.Exec(t, `UPDATE issue SET title = 'changed later', status = 'in_review' WHERE id = $1`, f.issueID)
			retry := f.move(t, body, nil).Want(http.StatusOK)

			if retry.Map()["status"] != "in_progress" || retry.Map()["title"] != first.Map()["title"] {
				t.Fatalf("retry = status %v title %v, want the first result (in_progress, %v)", retry.Map()["status"], retry.Map()["title"], first.Map()["title"])
			}
			runs := 0
			if kind == "agent" {
				runs = 1
			}
			f.wantCounts(t, 1, 1, runs)
		})
	}
}

// Review finding P1 (silent-guard): a changed field outside the handoff block
// must not be dropped silently. Any changed body field with the same key is refused.
func TestHandoffOperation_ChangedAnyFieldRefused(t *testing.T) {
	changes := map[string]map[string]any{
		"title":        {"title": "another title"},
		"priority":     {"priority": "urgent"},
		"suppress_run": {"suppress_run": true},
		"description":  {"description": "another description"},
	}
	for name, extra := range changes {
		t.Run(name, func(t *testing.T) {
			f := newOpFixture(t, "member")
			key := uuid.NewString()
			f.move(t, commentBody(key, "same text"), nil).Want(http.StatusOK)
			c, h, r := f.counts(t)

			changed := commentBody(key, "same text")
			for k, v := range extra {
				changed[k] = v
			}
			wantOpError(t, f.move(t, changed, nil), http.StatusConflict, "operation_key_reused")
			f.wantCounts(t, c, h, r)
		})
	}
}

// Review finding P1 round 2 (failure-path): the stored first response must be
// the whole response, not the bare issue. A chained handoff adds step_brief after
// the commit; with the post-commit store lost, the retry still returns it.
func TestHandoffOperation_LostStoreRetryReturnsWholeChainedResponse(t *testing.T) {
	f := newChainFixture(t, chainCase{flag: true})
	failPostCommitStore(t, f.issueID)
	t.Cleanup(func() { cleanupOperations(f.issueID) })
	body := map[string]any{"status": "in_progress", "operation_key": uuid.NewString()}

	first := f.moveAs(t, f.runHeaders(), body).Want(http.StatusOK)
	if stepBriefOf(t, first) == "" {
		t.Fatalf("first response has no step_brief; the fixture does not chain: %s", first.Body.String())
	}
	dbfx.Exec(t, `UPDATE issue SET title = 'changed later' WHERE id = $1`, f.issueID)
	retry := f.moveAs(t, f.runHeaders(), body).Want(http.StatusOK)

	if !reflect.DeepEqual(first.Map(), retry.Map()) {
		t.Fatalf("retry differs from the first response:\nfirst: %s\nretry: %s", first.Body.String(), retry.Body.String())
	}
}
