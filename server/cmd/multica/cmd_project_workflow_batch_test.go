package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// CHE-10137: `project workflow set <project>... <workflow>` with two or more
// projects sends one batch request, so the server opens one owner card.

const testProjectUUID2 = "66666666-6666-6666-6666-666666666666"

func batchTestCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringArray("map", nil, "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().String("output", "json", "")
	return cmd
}

// batchServer answers the workflow lookup and the batch call with batchStatus.
func batchServer(t *testing.T, batchStatus int, batchBody string) *[]recordedRequest {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{Method: r.Method, Path: r.URL.Path}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&rec.Body)
		}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issue-workflows/"+testWorkflowUUID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testWorkflowUUID, "name": "Product", "steps": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/workflow":
			w.WriteHeader(batchStatus)
			_, _ = w.Write([]byte(batchBody))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/workflow"):
			_, _ = w.Write([]byte(`{"updated_issues":0}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)
	return &reqs
}

func TestProjectWorkflowSetManyProjectsSendsOneBatchRequest(t *testing.T) {
	reqs := batchServer(t, http.StatusAccepted, `{"action_id":"a","approve_url":"/x?approve=a"}`)
	out, err := captureStdout(t, func() error {
		return runProjectWorkflowSet(batchTestCmd(), []string{testProjectUUID, testProjectUUID2, testWorkflowUUID})
	})
	if err != nil {
		t.Fatal(err)
	}
	post := lastRequest(t, *reqs, http.MethodPost)
	if post.Path != "/api/projects/workflow" {
		t.Fatalf("path = %s, want the batch endpoint", post.Path)
	}
	ids, _ := post.Body["project_ids"].([]any)
	if len(ids) != 2 || ids[0] != testProjectUUID || ids[1] != testProjectUUID2 || post.Body["workflow_id"] != testWorkflowUUID {
		t.Fatalf("body = %v", post.Body)
	}
	if !strings.Contains(out, "approve_url") {
		t.Fatalf("stdout %q lacks the card link", out)
	}
}

func TestProjectWorkflowSetBatchFallsBackToDirectSwitches(t *testing.T) {
	reqs := batchServer(t, http.StatusConflict, `{"code":"workflow_batch_direct","error":"none of these switches needs owner approval"}`)
	if _, err := captureStdout(t, func() error {
		return runProjectWorkflowSet(batchTestCmd(), []string{testProjectUUID, testProjectUUID2, testWorkflowUUID})
	}); err != nil {
		t.Fatal(err)
	}
	direct := 0
	for _, r := range *reqs {
		if r.Method == http.MethodPost && r.Path != "/api/projects/workflow" {
			direct++
		}
	}
	if direct != 2 {
		t.Fatalf("direct switches = %d, want 2", direct)
	}
}

func TestProjectWorkflowSetBatchRefusesDryRun(t *testing.T) {
	batchServer(t, http.StatusAccepted, `{}`)
	cmd := batchTestCmd()
	_ = cmd.Flags().Set("dry-run", "true")
	if err := runProjectWorkflowSet(cmd, []string{testProjectUUID, testProjectUUID2, testWorkflowUUID}); err == nil {
		t.Fatal("--dry-run with two projects = nil error, want error")
	}
}
