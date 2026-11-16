package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPutMergeReceiptStoresFactsForTheCurrentHeadAndBaseOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, snapshot_head_sha, snapshot_base_ref)
		VALUES ($1, 1, 'fixture', 'receipt', 559401, 'receipt PR', 'open', 'https://example.test/pr', now(), now(), 'h1', 'h1', 'main')`, testWorkspaceID); err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM merge_receipt_fact WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1 AND repo_owner = 'fixture' AND repo_name = 'receipt'`, testWorkspaceID)
	})
	put := func(mutate map[string]any) int {
		body := map[string]any{"repo_owner": "fixture", "repo_name": "receipt", "pr_number": 559401, "head_sha": "h1", "base_ref": "main",
			"tier": 1, "security_gate": "success", "paths": []string{"a.go"}, "evidence": "receipt-1"}
		for k, v := range mutate {
			body[k] = v
		}
		w := httptest.NewRecorder()
		testHandler.PutMergeReceipt(w, newRequest("PUT", "/api/github/merge-receipts", body))
		return w.Code
	}
	rows := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM merge_receipt_fact WHERE workspace_id=$1 AND repo_name='receipt'`, testWorkspaceID)
	}
	if got := put(map[string]any{"head_sha": "old"}); got != http.StatusConflict || rows() != 0 {
		t.Fatalf("stale head: status %d rows %d, want 409 and 0", got, rows())
	}
	if got := put(map[string]any{"base_ref": "release"}); got != http.StatusConflict || rows() != 0 {
		t.Fatalf("other base: status %d rows %d, want 409 and 0", got, rows())
	}
	if got := put(map[string]any{"evidence": ""}); got != http.StatusBadRequest || rows() != 0 {
		t.Fatalf("no evidence: status %d rows %d, want 400 and 0", got, rows())
	}
	if got := put(nil); got != http.StatusNoContent || rows() != 1 {
		t.Fatalf("current head and base: status %d rows %d, want 204 and 1", got, rows())
	}
}

// A veto recorded in Multica drops the receipt and refuses a new one until it is lifted.
func TestMergeVetoHoldsReceiptsUntilLifted(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, snapshot_head_sha, snapshot_base_ref)
		VALUES ($1, 1, 'fixture', 'veto', 559402, 'veto PR', 'open', 'https://example.test/pr', now(), now(), 'h1', 'h1', 'main')`, testWorkspaceID); err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM merge_receipt_fact WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM merge_veto WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1 AND repo_owner = 'fixture' AND repo_name = 'veto'`, testWorkspaceID)
	})
	pr := map[string]any{"repo_owner": "fixture", "repo_name": "veto", "pr_number": 559402}
	call := func(fn func(http.ResponseWriter, *http.Request), method, path string, body map[string]any) int {
		w := httptest.NewRecorder()
		fn(w, newRequest(method, path, body))
		return w.Code
	}
	receipt := map[string]any{"repo_owner": "fixture", "repo_name": "veto", "pr_number": 559402, "head_sha": "h1", "base_ref": "main", "evidence": "r"}
	rows := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM merge_receipt_fact WHERE workspace_id=$1 AND repo_name='veto'`, testWorkspaceID)
	}
	if got := call(testHandler.PutMergeReceipt, "PUT", "/api/github/merge-receipts", receipt); got != http.StatusNoContent || rows() != 1 {
		t.Fatalf("receipt before the veto: status %d rows %d, want 204 and 1", got, rows())
	}
	if got := call(testHandler.PutMergeVeto, "PUT", "/api/github/merge-vetoes", pr); got != http.StatusNoContent || rows() != 0 {
		t.Fatalf("veto: status %d rows %d, want 204 and 0 receipts", got, rows())
	}
	if got := call(testHandler.PutMergeReceipt, "PUT", "/api/github/merge-receipts", receipt); got != http.StatusConflict || rows() != 0 {
		t.Fatalf("receipt under the veto: status %d rows %d, want 409 and 0", got, rows())
	}
	if got := call(testHandler.PutMergeVeto, "PUT", "/api/github/merge-vetoes", map[string]any{"repo_owner": "fixture"}); got != http.StatusBadRequest {
		t.Fatalf("veto with no PR: status %d, want 400", got)
	}
	if got := call(testHandler.DeleteMergeVeto, "DELETE", "/api/github/merge-vetoes", pr); got != http.StatusNoContent || rows() != 0 {
		t.Fatalf("lift: status %d rows %d, want 204 and 0 (the old receipt stays gone)", got, rows())
	}
	if got := call(testHandler.PutMergeReceipt, "PUT", "/api/github/merge-receipts", receipt); got != http.StatusNoContent || rows() != 1 {
		t.Fatalf("receipt after the lift: status %d rows %d, want 204 and 1", got, rows())
	}
}
