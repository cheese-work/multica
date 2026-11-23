package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9267 (plan CHE-9122, step 2): RecordStatusChangeWithEvent writes a
// receipt for every recorded server move whose cause kind is neither ui_or_api
// nor workflow_remap. Setting auto_move_receipt: unset is warn (one would-post
// activity naming the move), block (one system comment and one posted
// activity), off (nothing). These tests drive each writer of the plan's
// "Receipt writer inventory" through its real entry point.

// setReceiptMode stores settings.auto_move_receipt.mode for the test workspace;
// "" removes the key (unset).
func setReceiptMode(t *testing.T, mode string) {
	t.Helper()
	if mode == "" {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'auto_move_receipt' WHERE id = $1`, testWorkspaceID)
	} else {
		dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_set(COALESCE(settings,'{}'), '{auto_move_receipt}', jsonb_build_object('mode', $2::text)) WHERE id = $1`, testWorkspaceID, mode)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'auto_move_receipt' WHERE id = $1`, testWorkspaceID)
	})
}

// eligibleMove is one status_changed row a receipt is owed for.
type eligibleMove struct{ id, from, to, kind string }

// eligibleMoves lists the issue's status_changed rows whose cause kind is
// neither ui_or_api nor workflow_remap, oldest first.
func eligibleMoves(t *testing.T, issueID string) []eligibleMove {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT id::text, details->>'from', details->>'to', details->'trigger'->'cause'->>'kind'
		FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'
		  AND details->'trigger'->'cause'->>'kind' NOT IN ('ui_or_api', 'workflow_remap')
		ORDER BY created_at, id`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []eligibleMove
	for rows.Next() {
		var m eligibleMove
		if err := rows.Scan(&m.id, &m.from, &m.to, &m.kind); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// receiptActivities returns details->>'move_id' and details->>'comment_id' of
// every activity of the action on the issue, oldest first.
func receiptActivities(t *testing.T, issueID, action string) (moveIDs, commentIDs []string) {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT COALESCE(details->>'move_id',''), COALESCE(details->>'comment_id','')
		FROM activity_log WHERE issue_id = $1 AND action = $2 ORDER BY created_at, id`, issueID, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var move, comment string
		if err := rows.Scan(&move, &comment); err != nil {
			t.Fatal(err)
		}
		moveIDs, commentIDs = append(moveIDs, move), append(commentIDs, comment)
	}
	return
}

// receiptSystemComments returns id and content of the issue's system comments
// that name one of its status_changed rows (a receipt names its move), oldest
// first. Other system comments, such as a run-budget refusal, are not receipts.
func receiptSystemComments(t *testing.T, issueID string) (ids, contents []string) {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT c.id::text, c.content FROM comment c
		WHERE c.issue_id = $1 AND c.author_type = 'system' AND c.type = 'system'
		  AND EXISTS (SELECT 1 FROM activity_log a WHERE a.issue_id = c.issue_id
		              AND a.action = 'status_changed' AND position(a.id::text IN c.content) > 0)
		ORDER BY c.created_at, c.id`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			t.Fatal(err)
		}
		ids, contents = append(ids, id), append(contents, content)
	}
	return
}

func sameStrings(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

// wantReceipts checks the receipt state of issueID against its eligible moves:
//
//	warn:  one would-post row per eligible move naming its id; no comment, no posted row.
//	block: one system comment and one posted row per eligible move.
//	off:   nothing.
func wantReceipts(t *testing.T, issueID, mode string, wantMoves int) []eligibleMove {
	t.Helper()
	moves := eligibleMoves(t, issueID)
	if len(moves) != wantMoves {
		t.Fatalf("eligible status_changed rows = %d, want %d", len(moves), wantMoves)
	}
	var wantIDs []string
	for _, m := range moves {
		wantIDs = append(wantIDs, m.id)
	}
	wouldPost, _ := receiptActivities(t, issueID, "auto_move_receipt_would_post")
	posted, postedComments := receiptActivities(t, issueID, "auto_move_receipt_posted")
	commentIDs, contents := receiptSystemComments(t, issueID)
	switch mode {
	case "", "warn":
		if !sameStrings(wouldPost, wantIDs) {
			t.Fatalf("would-post rows name moves %v, want %v", wouldPost, wantIDs)
		}
		if len(posted) != 0 || len(commentIDs) != 0 {
			t.Fatalf("warn wrote %d posted rows and %d system comments, want 0 and 0", len(posted), len(commentIDs))
		}
	case "block":
		if len(wouldPost) != 0 {
			t.Fatalf("block wrote %d would-post rows, want 0", len(wouldPost))
		}
		if !sameStrings(posted, wantIDs) || !sameStrings(postedComments, commentIDs) {
			t.Fatalf("posted rows name moves %v and comments %v, want moves %v and comments %v", posted, postedComments, wantIDs, commentIDs)
		}
		for i, m := range moves {
			for _, want := range []string{m.id, m.from, m.to, m.kind} {
				if !strings.Contains(contents[i], want) {
					t.Fatalf("receipt %d lacks %q:\n%s", i, want, contents[i])
				}
			}
		}
	case "off":
		if len(wouldPost)+len(posted)+len(commentIDs) != 0 {
			t.Fatalf("off wrote %d would-post, %d posted, %d comments, want none", len(wouldPost), len(posted), len(commentIDs))
		}
	}
	return moves
}

// receiptWriter drives one writer of the inventory and returns the issue it moved.
type receiptWriter struct {
	name  string
	moves int // eligible status_changed rows the drive leaves
	drive func(t *testing.T) string
}

func receiptWriters() []receiptWriter {
	return []receiptWriter{
		{"workflow_edge", 1, func(t *testing.T) string {
			c := newEdgeCase(t, 9267001, mergeEdgeSteps)
			linkedPRForWakeup(t, c.fx, 9267001, 926701, "rcpt-edge")
			setChildPR(t, c.fx.child.ID, "merged")
			c.enter(t, "in_progress")
			tickEdges(t)
			return c.issueID
		}},
		{"ci_advance", 1, func(t *testing.T) string {
			issueID, _, refresh, _ := ciAdvanceCase(t, 9267002, 926702, "rcpt-ci")
			refresh()
			return issueID
		}},
		{"run_budget", 1, func(t *testing.T) string {
			c := newEdgeCase(t, 9267003, mergeEdgeSteps)
			exhaustRunBudget(t, c)
			waitForExhaustedActivity(t, c.issueID)
			return c.issueID
		}},
		{"pr_merged_direct", 1, func(t *testing.T) string {
			const installationID int64 = 9267004
			t.Setenv("GITHUB_WEBHOOK_SECRET", "che9267-secret")
			steps := []map[string]any{
				{"status_key": "backlog"}, {"status_key": "todo"}, {"status_key": "in_progress"},
				{"status_key": "in_review"}, {"status_key": "merged"}, {"status_key": "blocked"},
			}
			issueID, identifier, _ := prMergeIssueInReview(t, steps, installationID)
			setWorkspacePRMergeStatusForTest(t, "merged")
			firePRWebhook(t, "che9267-secret", installationID, 3, identifier+": work", "", "feat/che9267", "merged")
			return issueID
		}},
		{"decision_park_and_restore", 2, func(t *testing.T) string {
			fixture := newParkFixture(t)
			if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input()); err != nil {
				t.Fatal(err)
			}
			dbfx.Exec(t, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'lane_archived' WHERE issue_id = $1`, fixture.issueID)
			parked, _ := testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
			if err := testHandler.checkDecisionWait(context.Background(), parked); err != nil {
				t.Fatal(err)
			}
			return fixture.issueID
		}},
		{"approved_project_move", 1, func(t *testing.T) string {
			move := newPendingStatusMove(t, "backlog", "todo", "in_progress", "done")
			if out := move.approveAccepted(t); out.State != "executed" {
				t.Fatalf("outcome = %+v; want executed", out)
			}
			return move.issueID
		}},
		{"auto_close_after_merge", 2, func(t *testing.T) string {
			c := newCloseOrDispatchEdgeEntry(t)
			c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
			c.mergedPR(t, autoCloseHead, "main", "main")
			tickEdges(t)
			c.wantClosed(t)
			return c.issueID
		}},
	}
}

// (a) Every writer: in warn one would-post row naming the move, in block one
// receipt and one posted row, in off nothing, unset is warn.
func TestAutoMoveReceiptEveryWriter(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, w := range receiptWriters() {
		for _, mode := range []string{"", "warn", "block", "off"} {
			t.Run(w.name+"/"+modeName(mode), func(t *testing.T) {
				setReceiptMode(t, mode)
				issueID := w.drive(t)
				wantReceipts(t, issueID, mode, w.moves)
			})
		}
	}
}

func modeName(mode string) string {
	if mode == "" {
		return "unset"
	}
	return mode
}

// (b) A person's or an agent's direct move and a workflow remap write no
// receipt in any mode.
func TestAutoMoveReceiptSkipsDirectMovesAndRemaps(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, mode := range []string{"warn", "block"} {
		t.Run("member/"+mode, func(t *testing.T) {
			setReceiptMode(t, mode)
			issueID := createIssueForTimeline(t, "receipt member move")
			testutil.Call(t, testHandler.UpdateIssue, patchStatus(issueID, "in_progress")).Want(http.StatusOK)
			if n := len(statusRowIDs(t, issueID)); n != 1 {
				t.Fatalf("status_changed rows = %d, want 1", n)
			}
			wantReceipts(t, issueID, mode, 0)
		})
		t.Run("agent/"+mode, func(t *testing.T) {
			setReceiptMode(t, mode)
			issueID := createIssueForTimeline(t, "receipt agent move")
			putIssue(t, issueID, map[string]any{"status": "in_progress"}, func(r *http.Request) *http.Request { return asAgentTask(t, r) }).Want(http.StatusOK)
			if n := len(statusRowIDs(t, issueID)); n != 1 {
				t.Fatalf("status_changed rows = %d, want 1", n)
			}
			wantReceipts(t, issueID, mode, 0)
		})
		t.Run("workflow_remap/"+mode, func(t *testing.T) {
			setReceiptMode(t, mode)
			seedTestCatalog(t)
			resetDefaultWorkflow(t)
			projectID := createWorkflowTestProject(t, "Receipt remap project")
			issueID := dbfx.Issue(t, "receipt remapped", testutil.Cols{"project_id": projectID, "status": "in_review"})
			wf := createTestWorkflow(t, "Receipt remap "+workflowTestSuffix())
			setProjectWorkflow(t, projectID, map[string]any{
				"workflow_id": wf.ID, "status_mapping": map[string]string{"in_review": "done"},
			}).Want(http.StatusOK)
			wantRemapRow(t, issueID, "in_review", "done")
			wantReceipts(t, issueID, mode, 0)
		})
	}
}

// (d) A merge in block: two receipts (merged, then done) and no run.
func TestAutoMoveReceiptMergeInBlockPostsTwoReceiptsAndStartsNoRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setReceiptMode(t, "block")
	c := newCloseOrDispatchEdgeEntry(t)
	c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
	c.mergedPR(t, autoCloseHead, "main", "main")
	tickEdges(t)
	c.wantClosed(t)

	moves := wantReceipts(t, c.issueID, "block", 2)
	if moves[0].to != "merged" || moves[1].to != "done" {
		t.Fatalf("moves = %+v, want merged then done", moves)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, c.issueID); n != 0 {
		t.Fatalf("runs = %d after the receipts, want 0: a receipt starts no run", n)
	}
}

// failInsert makes inserts matching when on table fail for the test.
func failInsert(t *testing.T, name, table, when string) {
	t.Helper()
	fn := "che9267_fail_" + name
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected receipt failure'; END $$`, fn),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION %s()`, fn, table, when, fn),
	} {
		dbfx.Exec(t, stmt)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, fn, table))
		testPool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
}

// (f) REQUIRED_TEST FP-03 (plan hash ffcd9835…07ed): an insert error after the
// status and status_changed writes of a decision park fails the park and leaves
// no status change, no status_changed row, no receipt, no would-post and no
// posted row.
func TestAutoMoveReceiptInsertFailureRollsBackDecisionPark(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cases := map[string]func(t *testing.T, issueID string){
		"block": func(t *testing.T, issueID string) {
			failInsert(t, "comment", "comment", fmt.Sprintf(`NEW.issue_id = '%s' AND NEW.author_type = 'system' AND NEW.type = 'system'`, issueID))
		},
		"warn": func(t *testing.T, issueID string) {
			failInsert(t, "wouldpost", "activity_log", fmt.Sprintf(`NEW.issue_id = '%s' AND NEW.action = 'auto_move_receipt_would_post'`, issueID))
		},
	}
	for mode, inject := range cases {
		t.Run(mode, func(t *testing.T) {
			setReceiptMode(t, mode)
			fixture := newParkFixture(t)
			inject(t, fixture.issueID)

			if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input()); err == nil {
				t.Fatal("the park reported success although the receipt insert failed")
			}

			if status, _, parkedFrom := fixture.state(t); status != "in_progress" || parkedFrom != "" {
				t.Fatalf("status = %q, parked_from = %q after the failed park, want in_progress and no park record", status, parkedFrom)
			}
			if n := len(statusRowIDs(t, fixture.issueID)); n != 0 {
				t.Fatalf("status_changed rows = %d, want 0", n)
			}
			for _, action := range []string{"auto_move_receipt_would_post", "auto_move_receipt_posted"} {
				if moves, _ := receiptActivities(t, fixture.issueID, action); len(moves) != 0 {
					t.Fatalf("%s rows = %d, want 0", action, len(moves))
				}
			}
			if ids, _ := receiptSystemComments(t, fixture.issueID); len(ids) != 0 {
				t.Fatalf("system comments = %d, want 0", len(ids))
			}
		})
	}
}
