package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// read_snapshot_approval (CHE-2216 plan 3.4, 3.7, slice B3): a person approves the
// exact bytes of one object for one issue. T29 (CI log) and T17 (i) (Multica body
// written by an external_read task) read this record to decide stub or full text.

func postReadApproval(t *testing.T, issueID string, body map[string]any, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("POST", "/api/issues/"+issueID+"/read-approvals", body), "id", issueID)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, testHandler.ApproveReadSnapshot, req)
}

func approved(t *testing.T, issueID, kind, objectID, hash string) bool {
	t.Helper()
	ok, err := testHandler.hasReadSnapshotApproval(context.Background(), util.MustParseUUID(testWorkspaceID), util.MustParseUUID(issueID), kind, objectID, hash)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestReadSnapshotApprovalIsScopedToIssueObjectAndExactBytes(t *testing.T) {
	issueID := dbfx.Issue(t, "read approval target")
	otherIssue := dbfx.Issue(t, "read approval other")
	log := sha256Hex("ci log bytes")
	postReadApproval(t, issueID, map[string]any{"object_kind": "ci_log", "object_id": "run-77", "body_hash": log}, nil).Want(http.StatusCreated)

	if !approved(t, issueID, "ci_log", "run-77", log) {
		t.Fatal("approved bytes not found")
	}
	for name, ok := range map[string]bool{
		"changed bytes": approved(t, issueID, "ci_log", "run-77", sha256Hex("ci log bytes, edited")),
		"other object":  approved(t, issueID, "ci_log", "run-78", log),
		"other kind":    approved(t, issueID, "pr_body", "run-77", log),
		"other issue":   approved(t, otherIssue, "ci_log", "run-77", log),
	} {
		if ok {
			t.Fatalf("%s must not be approved", name)
		}
	}
	// Approving the same bytes again is a no-op, not an error.
	postReadApproval(t, issueID, map[string]any{"object_kind": "ci_log", "object_id": "run-77", "body_hash": log}, nil).Want(http.StatusCreated)
	if n := dbfx.Count(t, `SELECT count(*) FROM read_snapshot_approval WHERE issue_id = $1`, issueID); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	var memberUser string
	dbfx.QueryRow(t, `SELECT approver_user_id::text FROM read_snapshot_approval WHERE issue_id = $1`, issueID).Scan(&memberUser)
	if memberUser != testUserID {
		t.Fatalf("approver = %s, want %s", memberUser, testUserID)
	}
}

func TestReadSnapshotApprovalRefusesMachinesAndPlainMembers(t *testing.T) {
	issueID := dbfx.Issue(t, "read approval refusals")
	body := map[string]any{"object_kind": "pr_body", "object_id": "PR_1", "body_hash": sha256Hex("x")}
	agentID := seededReadyAgentID(t)
	taskID := createHandlerTestTaskForAgent(t, agentID)
	postReadApproval(t, issueID, body, func(r *http.Request) {
		r.Header.Set("X-Agent-ID", agentID)
		r.Header.Set("X-Task-ID", taskID)
	}).Want(http.StatusForbidden)
	postReadApproval(t, issueID, body, func(r *http.Request) {
		r.Header.Set("X-Actor-Source", "task_token")
		r.Header.Set("X-Agent-ID", agentID)
	}).Want(http.StatusForbidden)
	userID := dbfx.User(t, "Plain Member", "plain-member-read-approval@example.com")
	dbfx.Member(t, testWorkspaceID, userID, "member")
	postReadApproval(t, issueID, body, func(r *http.Request) { r.Header.Set("X-User-ID", userID) }).Want(http.StatusForbidden)
	if n := dbfx.Count(t, `SELECT count(*) FROM read_snapshot_approval WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("refused writes left %d rows", n)
	}
}

func TestReadSnapshotApprovalValidation(t *testing.T) {
	issueID := dbfx.Issue(t, "read approval validation")
	good := func() map[string]any {
		return map[string]any{"object_kind": "commit_message", "object_id": "abc", "body_hash": sha256Hex("m")}
	}
	mutate := func(k string, v any) map[string]any { b := good(); b[k] = v; return b }
	for name, body := range map[string]map[string]any{
		"unknown kind":   mutate("object_kind", "pr_comment"),
		"empty object":   mutate("object_id", ""),
		"short hash":     mutate("body_hash", "abc"),
		"uppercase hash": mutate("body_hash", "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"),
		"unknown field":  mutate("approver_user_id", testUserID),
	} {
		t.Run(name, func(t *testing.T) { postReadApproval(t, issueID, body, nil).Want(http.StatusBadRequest) })
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM read_snapshot_approval WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("invalid writes left %d rows", n)
	}
}

// T29 (CI log) and T17 (i): a grant created with approve_current_comments approves the
// issue's comments as they are now, by body hash. A later comment or an edited body is not covered.
func TestGrantCanApproveCurrentCommentSet(t *testing.T) {
	issueID := dbfx.Issue(t, "grant comment set")
	first := dbfx.Comment(t, issueID, "first comment")
	body := validGrantBody()
	body["approve_current_comments"] = true
	postGrant(t, issueID, body, nil).Want(http.StatusCreated)

	if !approved(t, issueID, "multica_comment", first, sha256Hex("first comment")) {
		t.Fatal("existing comment not approved at grant time")
	}
	if approved(t, issueID, "multica_comment", first, sha256Hex("first comment, edited")) {
		t.Fatal("edited body must not be approved")
	}
	later := dbfx.Comment(t, issueID, "written after the grant")
	if approved(t, issueID, "multica_comment", later, sha256Hex("written after the grant")) {
		t.Fatal("a comment written after the grant must not be approved")
	}
}

func TestGrantWithoutFlagApprovesNothing(t *testing.T) {
	issueID := dbfx.Issue(t, "grant no comment set")
	dbfx.Comment(t, issueID, "a comment")
	postGrant(t, issueID, validGrantBody(), nil).Want(http.StatusCreated)
	if n := dbfx.Count(t, `SELECT count(*) FROM read_snapshot_approval WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
}
