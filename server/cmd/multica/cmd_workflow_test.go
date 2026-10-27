package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
