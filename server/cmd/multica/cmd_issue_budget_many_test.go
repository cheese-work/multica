package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

var manyIssueUUIDs = []string{
	"11111111-1111-1111-1111-111111111111",
	"22222222-2222-2222-2222-222222222222",
	"33333333-3333-3333-3333-333333333333",
}

// manyBudgetServer serves a budget snapshot for every issue in
// manyIssueUUIDs and records every batch, single-proposal and PAT request POST.
func manyBudgetServer(t *testing.T, batches, singles, requests *[]map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/run-budget"):
			_ = json.NewEncoder(w).Encode(map[string]any{"budgeted": true, "window_id": "w-" + r.URL.Path[len("/api/issues/"):len("/api/issues/")+1], "raise_preview": map[string]any{"limit_after": 55}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/ws-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"slug": "acme"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/decision-actions/proposals/batch":
			*batches = append(*batches, body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"batches": []map[string]any{{
				"batch_id": "batch-1", "action_type": "issue.run_budget.top_up", "replayed": false,
				"requests": []map[string]any{
					{"id": "req-1", "chat_session_id": "dm-9", "batch_id": "batch-1", "action": map[string]any{"id": "act-1"}},
					{"id": "req-2", "chat_session_id": "dm-9", "batch_id": "batch-1", "action": map[string]any{"id": "act-2"}},
					{"id": "req-3", "chat_session_id": "dm-9", "batch_id": "batch-1", "action": map[string]any{"id": "act-3"}},
				},
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/decision-actions/proposals":
			*singles = append(*singles, body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "req-1", "action": map[string]any{"id": "act-1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/run-budget/requests"):
			*requests = append(*requests, map[string]any{"path": r.URL.Path, "body": body})
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"requests": []map[string]any{{"approve_url": "/acme/approve"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
}

func TestTopUpOfThreeIssuesCallsTheBatchRouteOnce(t *testing.T) {
	var batches, singles, requests []map[string]any
	manyBudgetServer(t, &batches, &singles, &requests)
	cmd := newIssueBudgetRaiseTestCmd()
	_ = cmd.Flags().Set("for", "user-1")
	out, err := captureStdout(t, func() error { return runIssueBudgetTopUp(cmd, manyIssueUUIDs) })
	if err != nil {
		t.Fatalf("top-up: %v", err)
	}
	if len(batches) != 1 || len(singles) != 0 {
		t.Fatalf("batches = %d singles = %d, want 1 batch call and no single call", len(batches), len(singles))
	}
	items, _ := batches[0]["items"].([]any)
	if len(items) != 3 || batches[0]["for"] != "user-1" || batches[0]["goal"] == "" {
		t.Fatalf("batch body = %v, want 3 items, for user-1 and a goal", batches[0])
	}
	first, _ := items[0].(map[string]any)
	wantParams := map[string]any{"issue_id": manyIssueUUIDs[0], "window_id": "w-1"}
	if first["action_type"] != "issue.run_budget.top_up" || first["issue_id"] != manyIssueUUIDs[0] || first["version"] != float64(1) || !reflect.DeepEqual(first["params"], wantParams) {
		t.Fatalf("first item = %v", first)
	}
	if strings.Count(out, "/acme/chat?session=dm-9") != 1 || !strings.Contains(out, "batch-1") {
		t.Fatalf("output %q wants one card link and the batch id", out)
	}
}

func TestTopUpBatchJSONReturnsTheBatchAndItemIDs(t *testing.T) {
	var batches, singles, requests []map[string]any
	manyBudgetServer(t, &batches, &singles, &requests)
	cmd := newIssueBudgetRaiseTestCmd()
	_ = cmd.Flags().Set("output", "json")
	_ = cmd.Flags().Set("goal", "Reopen the sync issues")
	out, err := captureStdout(t, func() error { return runIssueBudgetTopUp(cmd, manyIssueUUIDs) })
	if err != nil {
		t.Fatalf("top-up: %v", err)
	}
	for _, want := range []string{`"batch_id": "batch-1"`, `"req-1"`, `"req-2"`, `"req-3"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %s", out, want)
		}
	}
	if batches[0]["goal"] != "Reopen the sync issues" {
		t.Fatalf("goal = %v, want the --goal text", batches[0]["goal"])
	}
}

func TestTopUpOfOneIssueKeepsTheSingleProposal(t *testing.T) {
	var batches, singles, requests []map[string]any
	manyBudgetServer(t, &batches, &singles, &requests)
	if _, err := captureStdout(t, func() error { return runIssueBudgetTopUp(newIssueBudgetRaiseTestCmd(), manyIssueUUIDs[:1]) }); err != nil {
		t.Fatalf("top-up: %v", err)
	}
	if len(batches) != 0 || len(singles) != 1 {
		t.Fatalf("batches = %d singles = %d, want the unchanged single call", len(batches), len(singles))
	}
}

func TestRaiseOfManyIssuesBatchesWithEachIssuesSuggestedCap(t *testing.T) {
	var batches, singles, requests []map[string]any
	manyBudgetServer(t, &batches, &singles, &requests)
	if _, err := captureStdout(t, func() error { return runIssueBudgetRaise(newIssueBudgetRaiseTestCmd(), manyIssueUUIDs[:2]) }); err != nil {
		t.Fatalf("raise: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("batches = %v, want one batch call", batches)
	}
	items, _ := batches[0]["items"].([]any)
	first, _ := items[0].(map[string]any)
	if len(items) != 2 || first["action_type"] != "issue.run_budget.raise" || first["params"].(map[string]any)["new_cap"] != float64(55) {
		t.Fatalf("batches = %v", batches)
	}
}

func TestManyIssuesWithAPATRequestEachIssueOnItsOwn(t *testing.T) {
	var batches, singles, requests []map[string]any
	manyBudgetServer(t, &batches, &singles, &requests)
	asPAT(t)
	if _, err := captureStdout(t, func() error { return runIssueBudgetTopUp(newIssueBudgetRaiseTestCmd(), manyIssueUUIDs) }); err != nil {
		t.Fatalf("top-up: %v", err)
	}
	if len(batches) != 0 || len(singles) != 0 || len(requests) != 3 {
		t.Fatalf("batches = %d singles = %d requests = %d, want 3 per-issue requests", len(batches), len(singles), len(requests))
	}
}

func TestDuplicateIssueIDsJoinTheBatchOnce(t *testing.T) {
	var batches, singles, requests []map[string]any
	manyBudgetServer(t, &batches, &singles, &requests)
	args := []string{manyIssueUUIDs[0], manyIssueUUIDs[1], strings.ToUpper(manyIssueUUIDs[0])}
	if _, err := captureStdout(t, func() error { return runIssueBudgetTopUp(newIssueBudgetRaiseTestCmd(), args) }); err != nil {
		t.Fatalf("top-up: %v", err)
	}
	if items, _ := batches[0]["items"].([]any); len(items) != 2 {
		t.Fatalf("items = %v, want 2 after dropping the duplicate", items)
	}
}
