package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

const testWorkflowID = "22222222-2222-2222-2222-222222222222"

func workflowFixture() map[string]any {
	return map[string]any{
		"id":      testWorkflowID,
		"name":    "Delivery pilot",
		"version": "2026-10-23T00:00:00.123456789Z",
		"steps": []any{
			map[string]any{"status_key": "todo", "handler": map[string]any{"type": "agent", "id": "a1"}, "instructions": "S1 text", "next_status_key": "in_progress"},
			map[string]any{"status_key": "in_progress", "handler": map[string]any{"type": "member", "id": "m1"}, "instructions": "S2 text\nline two", "back_status_key": "todo"},
			map[string]any{"status_key": "done", "handler": map[string]any{"type": ""}, "instructions": ""},
		},
	}
}

func newWorkflowTestCmd(flags ...string) *cobra.Command {
	cmd := &cobra.Command{Use: "workflow"}
	cmd.Flags().String("output", "json", "")
	cmd.Flags().Bool("full-id", false, "")
	for _, f := range flags {
		cmd.Flags().String(f, "", "")
	}
	return cmd
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWorkflowGetPrintsSteps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/issue-workflows/"+testWorkflowID {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(workflowFixture())
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowTestCmd()
	out, err := captureStdout(t, func() error { return runWorkflowGet(cmd, []string{testWorkflowID}) })
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json output: %v\n%s", err, out)
	}
	if len(got["steps"].([]any)) != 3 {
		t.Fatalf("steps = %v", got["steps"])
	}

	_ = cmd.Flags().Set("output", "table")
	out, err = captureStdout(t, func() error { return runWorkflowGet(cmd, []string{testWorkflowID}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Delivery pilot", "in_progress", "agent:a1", "S1 text", "S2 text\nline two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestWorkflowListPrintsWorkflows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/issue-workflows" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workflows": []any{workflowFixture()}, "total": 1})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowTestCmd()
	_ = cmd.Flags().Set("output", "table")
	out, err := captureStdout(t, func() error { return runWorkflowList(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Delivery pilot", "in_progress", "agent:a1", "S1 text", "S2 text\nline two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list table missing %q:\n%s", want, out)
		}
	}
}

func TestWorkflowSetInstructionsChangesOnlyOneField(t *testing.T) {
	var patched map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(workflowFixture())
		case http.MethodPatch:
			if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
				t.Fatal(err)
			}
			resp := workflowFixture()
			resp["steps"] = patched["steps"]
			_ = json.NewEncoder(w).Encode(resp)
		default:
			t.Fatalf("method %s", r.Method)
		}
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowTestCmd("status", "file")
	_ = cmd.Flags().Set("status", "in_progress")
	_ = cmd.Flags().Set("file", writeTemp(t, "new S2\n"))
	if _, err := captureStdout(t, func() error { return runWorkflowSetInstructions(cmd, []string{testWorkflowID}) }); err != nil {
		t.Fatal(err)
	}

	want := workflowFixture()["steps"].([]any)
	want[1].(map[string]any)["instructions"] = "new S2\n"
	if !reflect.DeepEqual(patched["steps"], want) {
		t.Fatalf("steps sent = %v\nwant %v", patched["steps"], want)
	}
	if patched["expected_version"] != "2026-10-23T00:00:00.123456789Z" {
		t.Fatalf("expected_version = %v", patched["expected_version"])
	}
	if _, ok := patched["name"]; ok {
		t.Fatalf("name must not be sent: %v", patched)
	}
}

func TestWorkflowSetInstructionsRejectsZeroOrDuplicateMatchWithoutPatch(t *testing.T) {
	dup := workflowFixture()
	dup["steps"] = append(dup["steps"].([]any), dup["steps"].([]any)[0])
	for name, tc := range map[string]struct {
		wf     map[string]any
		status string
	}{
		"zero":      {workflowFixture(), "blocked"},
		"duplicate": {dup, "todo"},
	} {
		t.Run(name, func(t *testing.T) {
			var patches atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(tc.wf)
					return
				}
				patches.Add(1)
			}))
			defer srv.Close()
			setCLITestServerEnv(t, srv.URL)

			cmd := newWorkflowTestCmd("status", "file")
			_ = cmd.Flags().Set("status", tc.status)
			_ = cmd.Flags().Set("file", writeTemp(t, "x"))
			_, err := captureStdout(t, func() error { return runWorkflowSetInstructions(cmd, []string{testWorkflowID}) })
			if err == nil || !strings.Contains(err.Error(), tc.status) {
				t.Fatalf("err = %v, want error naming %q", err, tc.status)
			}
			if patches.Load() != 0 {
				t.Fatal("PATCH sent despite bad status match")
			}
		})
	}
}

func TestWorkflowUpdateSendsStepsFileAndExplains403(t *testing.T) {
	var patched map[string]any
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/issue-workflows/"+testWorkflowID {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&patched)
		w.WriteHeader(status)
		if status == http.StatusForbidden {
			_, _ = w.Write([]byte(`{"error":"insufficient permissions"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(workflowFixture())
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowTestCmd("steps-file")
	_ = cmd.Flags().Set("steps-file", writeTemp(t, `[{"status_key":"todo","instructions":"a"}]`))
	out, err := captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{testWorkflowID}) })
	if err != nil {
		t.Fatal(err)
	}
	if got := patched["steps"].([]any); len(got) != 1 || got[0].(map[string]any)["instructions"] != "a" {
		t.Fatalf("steps sent = %v", patched["steps"])
	}
	if !strings.Contains(out, "Delivery pilot") {
		t.Fatalf("readback not printed:\n%s", out)
	}

	status = http.StatusForbidden
	_, err = captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{testWorkflowID}) })
	if err == nil || !strings.Contains(err.Error(), "owner or admin") {
		t.Fatalf("err = %v, want owner/admin message", err)
	}
}

func TestWorkflowUpdateRejectsNonArrayFile(t *testing.T) {
	cmd := newWorkflowTestCmd("steps-file")
	_ = cmd.Flags().Set("steps-file", writeTemp(t, `{"steps":[]}`))
	if err := runWorkflowUpdate(cmd, []string{testWorkflowID}); err == nil {
		t.Fatal("want error for non-array steps file")
	}
}

func newWorkflowRulesCmd() *cobra.Command {
	cmd := newWorkflowTestCmd("steps-file")
	cmd.Flags().Int("max-runs-per-issue", 0, "")
	cmd.Flags().Bool("include-child-prs", true, "")
	return cmd
}

func TestWorkflowUpdateSendsOnlyTheRuleFlagsGiven(t *testing.T) {
	var patched map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		patched = nil
		_ = json.NewDecoder(r.Body).Decode(&patched)
		_ = json.NewEncoder(w).Encode(workflowFixture())
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowRulesCmd()
	_ = cmd.Flags().Set("max-runs-per-issue", "5")
	if _, err := captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{testWorkflowID}) }); err != nil {
		t.Fatalf("rule flag alone: %v", err)
	}
	if !reflect.DeepEqual(patched, map[string]any{"max_runs_per_issue": float64(5)}) {
		t.Fatalf("sent %v, want only max_runs_per_issue=5", patched)
	}

	cmd = newWorkflowRulesCmd()
	_ = cmd.Flags().Set("include-child-prs", "false")
	_ = cmd.Flags().Set("steps-file", writeTemp(t, `[{"status_key":"todo"}]`))
	if _, err := captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{testWorkflowID}) }); err != nil {
		t.Fatalf("steps and rule: %v", err)
	}
	if patched["include_child_prs"] != false || patched["steps"] == nil || patched["max_runs_per_issue"] != nil {
		t.Fatalf("sent %v, want steps and include_child_prs=false only", patched)
	}

	cmd = newWorkflowRulesCmd()
	_ = cmd.Flags().Set("max-runs-per-issue", "0")
	if _, err := captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{testWorkflowID}) }); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if v, ok := patched["max_runs_per_issue"]; !ok || v != float64(0) {
		t.Fatalf("sent %v, want max_runs_per_issue=0 to clear the cap", patched)
	}

	if err := runWorkflowUpdate(newWorkflowRulesCmd(), []string{testWorkflowID}); err == nil {
		t.Fatal("want error when neither --steps-file nor a rule flag is given")
	}
}

func TestWorkflowTablePrintsRunRules(t *testing.T) {
	fixture := workflowFixture()
	fixture["max_runs_per_issue"] = float64(7)
	fixture["include_child_prs"] = false
	out, _ := captureStdout(t, func() error { printWorkflowTable(fixture); return nil })
	for _, want := range []string{"max runs per issue: 7", "include child PRs: false"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
	delete(fixture, "max_runs_per_issue")
	fixture["include_child_prs"] = true
	out, _ = captureStdout(t, func() error { printWorkflowTable(fixture); return nil })
	if !strings.Contains(out, "max runs per issue: 15 (default)") {
		t.Fatalf("table missing the default cap:\n%s", out)
	}
}

const (
	testWorkflowUUID = "22222222-2222-2222-2222-222222222222"
	testDefaultUUID  = "33333333-3333-3333-3333-333333333333"
	testProjectUUID  = "44444444-4444-4444-4444-444444444444"
	testAgentUUID    = "55555555-5555-5555-5555-555555555555"
)

type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

// workflowTestServer serves a small fixed workspace: a Default workflow, a
// "Review flow" workflow, and one project that uses Default.
func workflowTestServer(t *testing.T) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	review := map[string]any{
		"id": testWorkflowUUID, "name": "Review flow", "is_default": false,
		"project_ids": []any{},
		"steps": []any{
			map[string]any{"status_key": "todo", "handler": map[string]any{"type": "none"}},
			map[string]any{
				"status_key": "in_review", "handler": map[string]any{"type": "agent", "id": testAgentUUID},
				"instructions": "Review the PR", "next_status_key": "done", "back_status_key": "todo",
			},
			map[string]any{"status_key": "done", "handler": map[string]any{"type": "none"}},
		},
	}
	def := map[string]any{
		"id": testDefaultUUID, "name": "Default", "is_default": true,
		"project_ids": []any{testProjectUUID}, "steps": []any{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{Method: r.Method, Path: r.URL.Path}
		if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodPut) {
			_ = json.NewDecoder(r.Body).Decode(&rec.Body)
		}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issue-workflows":
			_ = json.NewEncoder(w).Encode(map[string]any{"workflows": []any{def, review}, "total": 2})
		case r.Method == http.MethodGet && r.URL.Path == "/api/issue-workflows/"+testWorkflowUUID:
			_ = json.NewEncoder(w).Encode(review)
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/"+testProjectUUID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testProjectUUID, "title": "P", "workflow_id": nil})
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/"+testProjectUUID+"/workflow":
			_ = json.NewEncoder(w).Encode(map[string]any{"dry_run": true, "plan": map[string]any{"required": []any{}, "total_issues": 3, "affected_issues": 0}})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/issue-workflows/"+testWorkflowUUID+"/handoffs/"):
			_ = json.NewEncoder(w).Encode(review)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/issue-workflows/"+testWorkflowUUID:
			_ = json.NewEncoder(w).Encode(map[string]any{"dry_run": true, "plan": map[string]any{}})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/issue-workflows/"+testWorkflowUUID:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)
	return srv, &reqs
}

func lastRequest(t *testing.T, reqs []recordedRequest, method string) recordedRequest {
	t.Helper()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Method == method {
			return reqs[i]
		}
	}
	t.Fatalf("no %s request recorded; got %+v", method, reqs)
	return recordedRequest{}
}

func TestParseStatusMapping(t *testing.T) {
	got, err := parseStatusMapping([]string{"qa=in_review", " blocked = todo "})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["qa"] != "in_review" || got["blocked"] != "todo" || len(got) != 2 {
		t.Fatalf("mapping = %v", got)
	}
	for _, bad := range [][]string{{"qa"}, {"=todo"}, {"qa="}, {"qa=a", "qa=b"}} {
		if _, err := parseStatusMapping(bad); err == nil {
			t.Errorf("parseStatusMapping(%v) = nil error, want error", bad)
		}
	}
	if got, err := parseStatusMapping(nil); err != nil || got != nil {
		t.Fatalf("empty input = %v, %v; want nil, nil", got, err)
	}
}

func TestResolveWorkflowByNameDefaultAndPrefix(t *testing.T) {
	_, _ = workflowTestServer(t)
	cmd := &cobra.Command{}
	client, err := newAPIClient(cmd)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for input, want := range map[string]string{
		"default":        testDefaultUUID,
		"review FLOW":    testWorkflowUUID,
		"22222222":       testWorkflowUUID,
		testWorkflowUUID: testWorkflowUUID,
	} {
		wf, err := resolveWorkflow(ctx, client, input)
		if err != nil {
			t.Fatalf("resolveWorkflow(%q): %v", input, err)
		}
		if strVal(wf, "id") != want {
			t.Errorf("resolveWorkflow(%q) = %s, want %s", input, strVal(wf, "id"), want)
		}
	}
	if _, err := resolveWorkflow(ctx, client, "nope"); err == nil {
		t.Fatal("resolveWorkflow(nope) = nil error, want error")
	}
}

func TestProjectWorkflowSetSendsMappingAndDryRun(t *testing.T) {
	_, reqs := workflowTestServer(t)
	cmd := &cobra.Command{}
	cmd.Flags().StringArray("map", nil, "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().String("output", "json", "")
	_ = cmd.Flags().Set("map", "in_progress=in_review")
	_ = cmd.Flags().Set("dry-run", "true")

	if err := runProjectWorkflowSet(cmd, []string{testProjectUUID, "Review flow"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	post := lastRequest(t, *reqs, http.MethodPost)
	if post.Path != "/api/projects/"+testProjectUUID+"/workflow" {
		t.Fatalf("path = %s", post.Path)
	}
	if post.Body["workflow_id"] != testWorkflowUUID || post.Body["dry_run"] != true {
		t.Fatalf("body = %v", post.Body)
	}
	mapping, _ := post.Body["status_mapping"].(map[string]any)
	if mapping["in_progress"] != "in_review" {
		t.Fatalf("status_mapping = %v", post.Body["status_mapping"])
	}
}

func TestProjectWorkflowSetDefaultSendsNull(t *testing.T) {
	_, reqs := workflowTestServer(t)
	cmd := &cobra.Command{}
	cmd.Flags().StringArray("map", nil, "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().String("output", "json", "")

	if err := runProjectWorkflowSet(cmd, []string{testProjectUUID, "Default"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	post := lastRequest(t, *reqs, http.MethodPost)
	v, present := post.Body["workflow_id"]
	if !present || v != nil {
		t.Fatalf("workflow_id = %v (present=%v), want explicit null", v, present)
	}
	if _, ok := post.Body["dry_run"]; ok {
		t.Fatalf("dry_run sent without --dry-run: %v", post.Body)
	}
}

func TestWorkflowHandoffSetKeepsUnchangedFields(t *testing.T) {
	_, reqs := workflowTestServer(t)
	cmd := &cobra.Command{}
	cmd.Flags().String("handler", "", "")
	cmd.Flags().String("instructions", "", "")
	cmd.Flags().String("instructions-file", "", "")
	cmd.Flags().String("next", "", "")
	cmd.Flags().String("back", "", "")
	cmd.Flags().String("copy-for-project", "", "")
	cmd.Flags().String("output", "json", "")
	_ = cmd.Flags().Set("instructions", "Check tests too")
	_ = cmd.Flags().Set("back", "")

	if err := runWorkflowHandoffSet(cmd, []string{testWorkflowUUID, "in_review"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	put := lastRequest(t, *reqs, http.MethodPut)
	if put.Path != "/api/issue-workflows/"+testWorkflowUUID+"/handoffs/in_review" {
		t.Fatalf("path = %s", put.Path)
	}
	handler, _ := put.Body["handler"].(map[string]any)
	if handler["type"] != "agent" || handler["id"] != testAgentUUID {
		t.Fatalf("handler = %v, want the existing agent kept", put.Body["handler"])
	}
	if put.Body["instructions"] != "Check tests too" {
		t.Fatalf("instructions = %v", put.Body["instructions"])
	}
	if put.Body["next_status_key"] != "done" {
		t.Fatalf("next_status_key = %v, want kept", put.Body["next_status_key"])
	}
	if put.Body["back_status_key"] != "" {
		t.Fatalf("back_status_key = %v, want cleared", put.Body["back_status_key"])
	}
}

func TestWorkflowHandoffSetRejectsUnknownStep(t *testing.T) {
	_, _ = workflowTestServer(t)
	cmd := &cobra.Command{}
	cmd.Flags().String("handler", "", "")
	cmd.Flags().String("instructions", "", "")
	cmd.Flags().String("instructions-file", "", "")
	cmd.Flags().String("next", "", "")
	cmd.Flags().String("back", "", "")
	cmd.Flags().String("copy-for-project", "", "")
	cmd.Flags().String("output", "json", "")
	if err := runWorkflowHandoffSet(cmd, []string{testWorkflowUUID, "qa"}); err == nil {
		t.Fatal("want error for a status the workflow does not list")
	}
}

func TestWorkflowUpdateRequiresAField(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().String("steps-file", "", "")
	cmd.Flags().StringArray("map", nil, "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().String("output", "json", "")
	_ = cmd.Flags().Set("dry-run", "true")
	if err := runWorkflowUpdate(cmd, []string{testWorkflowUUID}); err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Fatalf("err = %v, want nothing to update", err)
	}
}

func TestWorkflowUpdateSendsPatch(t *testing.T) {
	_, reqs := workflowTestServer(t)
	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().String("steps-file", "", "")
	cmd.Flags().StringArray("map", nil, "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().String("output", "json", "")
	_ = cmd.Flags().Set("name", "Renamed")
	_ = cmd.Flags().Set("map", "qa=in_review")
	_ = cmd.Flags().Set("dry-run", "true")

	if err := runWorkflowUpdate(cmd, []string{testWorkflowUUID}); err != nil {
		t.Fatalf("run: %v", err)
	}
	patch := lastRequest(t, *reqs, http.MethodPatch)
	if patch.Body["name"] != "Renamed" || patch.Body["dry_run"] != true {
		t.Fatalf("body = %v", patch.Body)
	}
	if _, ok := patch.Body["steps"]; ok {
		t.Fatalf("steps sent without --steps-file: %v", patch.Body)
	}
}

func TestProjectWorkflowGetFallsBackToDefault(t *testing.T) {
	_, reqs := workflowTestServer(t)
	cmd := &cobra.Command{}
	cmd.Flags().String("output", "json", "")
	if err := runProjectWorkflowGet(cmd, []string{testProjectUUID}); err != nil {
		t.Fatalf("run: %v", err)
	}
	sawList := false
	for _, r := range *reqs {
		if r.Method == http.MethodGet && r.Path == "/api/issue-workflows" {
			sawList = true
		}
	}
	if !sawList {
		t.Fatalf("expected the Default workflow to be looked up from the list; got %+v", *reqs)
	}
}

// mappingRequiredServer answers every write with the server's 409
// workflow_status_mapping_required, listing two statuses to map.
func mappingRequiredServer(t *testing.T) {
	t.Helper()
	body := map[string]any{
		"error": `issues still use "qa"; map it to a status of the target workflow`,
		"code":  "workflow_status_mapping_required",
		"plan": map[string]any{
			"required": []any{
				map[string]any{"status_key": "qa", "issue_count": 2, "suggested_status_key": "in_review"},
				map[string]any{"status_key": "blocked", "issue_count": 1, "suggested_status_key": "todo"},
			},
			"unchanged":       []any{},
			"total_issues":    5,
			"affected_issues": 3,
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testWorkflowUUID, "name": "Review flow", "steps": []any{}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)
}

func newProjectWorkflowSetTestCmd(output string) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringArray("map", nil, "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().String("output", output, "")
	return cmd
}

func TestProjectWorkflowSetMappingRequiredPrintsFullPlanJSON(t *testing.T) {
	mappingRequiredServer(t)
	cmd := newProjectWorkflowSetTestCmd("json")
	out, err := captureStdout(t, func() error {
		return runProjectWorkflowSet(cmd, []string{testProjectUUID, testWorkflowUUID})
	})
	if !errors.Is(err, errSilent) {
		t.Fatalf("err = %v, want errSilent so the command exits non-zero without printing twice", err)
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", jerr, out)
	}
	if got["code"] != "workflow_status_mapping_required" {
		t.Fatalf("code = %v", got["code"])
	}
	plan, _ := got["plan"].(map[string]any)
	required, _ := plan["required"].([]any)
	if len(required) != 2 {
		t.Fatalf("plan.required has %d entries, want both statuses: %s", len(required), out)
	}
}

func TestWorkflowUpdateMappingRequiredPrintsPlanTable(t *testing.T) {
	mappingRequiredServer(t)
	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().String("steps-file", "", "")
	cmd.Flags().StringArray("map", nil, "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().String("output", "table", "")
	_ = cmd.Flags().Set("name", "Renamed")
	out, err := captureStdout(t, func() error {
		return runWorkflowUpdate(cmd, []string{testWorkflowUUID})
	})
	if !errors.Is(err, errSilent) {
		t.Fatalf("err = %v, want errSilent", err)
	}
	for _, want := range []string{"qa", "in_review", "blocked", "todo"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectWorkflowSetOtherConflictKeepsDefaultError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"only workspace admins and the project lead can change this project's workflow"}`))
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)
	cmd := newProjectWorkflowSetTestCmd("json")
	out, err := captureStdout(t, func() error {
		return runProjectWorkflowSet(cmd, []string{testProjectUUID, "default"})
	})
	if err == nil || errors.Is(err, errSilent) {
		t.Fatalf("err = %v, want the HTTP error for the default formatter", err)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
}

// The same plan with 45 statuses runs past 4 KiB; the command must still
// print all of it through the real HTTP client.
func TestProjectWorkflowSetMappingRequiredKeepsLargePlan(t *testing.T) {
	required := make([]any, 0, 45)
	for i := 0; i < 45; i++ {
		required = append(required, map[string]any{
			"status_key":           fmt.Sprintf("custom_status_with_a_long_key_%02d", i),
			"issue_count":          1,
			"suggested_status_key": "todo",
		})
	}
	payload, _ := json.Marshal(map[string]any{
		"error": "map every status", "code": "workflow_status_mapping_required",
		"plan": map[string]any{"required": required, "unchanged": []any{}, "total_issues": 45, "affected_issues": 45},
	})
	if len(payload) <= 4096 {
		t.Fatalf("fixture is %d bytes; it must exceed 4096", len(payload))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testWorkflowUUID, "name": "Review flow", "steps": []any{}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)

	cmd := newProjectWorkflowSetTestCmd("json")
	out, err := captureStdout(t, func() error {
		return runProjectWorkflowSet(cmd, []string{testProjectUUID, testWorkflowUUID})
	})
	if !errors.Is(err, errSilent) {
		t.Fatalf("err = %v, want errSilent", err)
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("stdout is not JSON: %v", jerr)
	}
	plan, _ := got["plan"].(map[string]any)
	if rows, _ := plan["required"].([]any); len(rows) != 45 {
		t.Fatalf("plan.required has %d rows, want 45", len(rows))
	}
}

func TestWorkflowUpdateSendsStartStatus(t *testing.T) {
	var patched map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		patched = nil
		_ = json.NewDecoder(r.Body).Decode(&patched)
		_ = json.NewEncoder(w).Encode(workflowFixture())
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	newCmd := func() *cobra.Command {
		cmd := newWorkflowRulesCmd()
		cmd.Flags().String("start-status", "", "")
		return cmd
	}
	cmd := newCmd()
	_ = cmd.Flags().Set("start-status", "in_progress")
	if _, err := captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{testWorkflowID}) }); err != nil {
		t.Fatalf("start status alone: %v", err)
	}
	if !reflect.DeepEqual(patched, map[string]any{"start_status_key": "in_progress"}) {
		t.Fatalf("sent %v, want only start_status_key=in_progress", patched)
	}

	cmd = newCmd()
	_ = cmd.Flags().Set("start-status", "")
	if _, err := captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{testWorkflowID}) }); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !reflect.DeepEqual(patched, map[string]any{"start_status_key": ""}) {
		t.Fatalf("sent %v, want start_status_key cleared", patched)
	}
}

func lintFixture() map[string]any {
	finding := func(rule, level, step string, waived bool) map[string]any {
		return map[string]any{"rule": rule, "level": level, "step": step, "message": rule + " message", "waived": waived}
	}
	return map[string]any{"workflows": []any{
		map[string]any{"workflow_id": testWorkflowID, "workflow": "Delivery pilot", "errors": 1, "warnings": 1, "infos": 0,
			"findings": []any{finding("WF003", "error", "agent_accepted", false), finding("WF009", "warning", "in_progress", false)}},
		map[string]any{"workflow_id": "33333333-3333-3333-3333-333333333333", "workflow": "Clean", "errors": 0, "warnings": 0, "infos": 0,
			"findings": []any{finding("WF005", "error", "todo", true)}},
	}, "total": 2}
}

func newLintTestCmd(all bool) *cobra.Command {
	cmd := &cobra.Command{Use: "lint"}
	cmd.Flags().String("output", "table", "")
	cmd.Flags().Bool("all", all, "")
	return cmd
}

func lintServer(t *testing.T, wantPath string, body any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != wantPath {
			t.Fatalf("unexpected %s %s, want %s", r.Method, r.URL.Path, wantPath)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)
}

func TestWorkflowLintAllFailsOnUnwaivedError(t *testing.T) {
	lintServer(t, "/api/issue-workflows/lint", lintFixture())
	cmd := newLintTestCmd(true)
	out, err := captureStdout(t, func() error { return runWorkflowLint(cmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "1 unwaived error") {
		t.Fatalf("want an error naming the 1 unwaived error, got %v", err)
	}
	for _, want := range []string{"Delivery pilot", "WF003", "agent_accepted", "WF009", "waived"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table lacks %q:\n%s", want, out)
		}
	}
}

func TestWorkflowLintJSONAndWaivedErrorPasses(t *testing.T) {
	fix := lintFixture()
	one := fix["workflows"].([]any)[1]
	lintServer(t, "/api/issue-workflows/33333333-3333-3333-3333-333333333333/lint", one)
	cmd := newLintTestCmd(false)
	_ = cmd.Flags().Set("output", "json")
	out, err := captureStdout(t, func() error { return runWorkflowLint(cmd, []string{"33333333-3333-3333-3333-333333333333"}) })
	if err != nil {
		t.Fatalf("a waived error must not fail the run: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["workflow"] != "Clean" {
		t.Fatalf("json output = %v (%v)\n%s", got, err, out)
	}
}

func TestWorkflowLintNeedsOneTarget(t *testing.T) {
	if err := runWorkflowLint(newLintTestCmd(false), nil); err == nil {
		t.Fatal("no id and no --all must fail")
	}
	if err := runWorkflowLint(newLintTestCmd(true), []string{testWorkflowID}); err == nil {
		t.Fatal("an id together with --all must fail")
	}
}

// A member-chosen workflow name or message must not reach another member's
// terminal as control characters.
func TestWorkflowLintTableStripsControlCharacters(t *testing.T) {
	results := []lintResult{{
		Workflow: "evil\x1b]0;pwned\x07name",
		Findings: []lintFinding{{Rule: "WF009", Level: "warning", Step: "todo\x1b[2J", Message: "bad\rmsg\x00"}},
	}}
	out, _ := captureStdout(t, func() error { printLintTable(results); return nil })
	for _, r := range out {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			t.Fatalf("control character %q reached the table:\n%q", r, out)
		}
	}
}
