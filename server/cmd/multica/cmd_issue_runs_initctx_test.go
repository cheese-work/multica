package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const initCtxIssueID = "1881a167-4bb6-4602-944b-f40ce4192fe6"

func runIssueRunsForTest(t *testing.T, runs []map[string]any, output string, initContext, siblings bool) (string, error) {
	t.Helper()
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/issues/"+initCtxIssueID+"/task-runs" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(runs)
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
	cmd := &cobra.Command{Use: "runs"}
	cmd.Flags().String("output", output, "")
	cmd.Flags().Bool("full-id", false, "")
	cmd.Flags().Bool("active", false, "")
	cmd.Flags().Bool("siblings", siblings, "")
	cmd.Flags().Bool("init-context", initContext, "")
	return captureStdout(t, func() error { return runIssueRuns(cmd, []string{initCtxIssueID}) })
}

func initCtxRun(id string, parts []map[string]any) map[string]any {
	return map[string]any{
		"id":     id,
		"status": "completed",
		"result": map[string]any{"init_context": map[string]any{"parts": parts}},
	}
}

func TestRunIssueRunsInitContextTable(t *testing.T) {
	names := []string{"instructions", "tools", "skills", "memory", "project", "task", "seed"}
	parts := make([]map[string]any, len(names))
	for i, n := range names {
		parts[i] = map[string]any{"part": n, "bytes": 400 * (i + 1), "tokens": 100 * (i + 1), "budget": 500}
	}
	parts[6]["notes"] = []string{"over budget by 200 est. tokens"}

	out, err := runIssueRunsForTest(t, []map[string]any{
		initCtxRun("aaaaaaaa-1111-4222-8333-444444444444", parts),
		{"id": "bbbbbbbb-1111-4222-8333-444444444444", "status": "completed", "result": map[string]any{"output": "x"}},
		{"id": "cccccccc-1111-4222-8333-444444444444", "status": "running"},
	}, "table", true, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"PART", "TOKENS", "BUDGET", "NOTES"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing header %q in:\n%s", want, out)
		}
	}
	for _, n := range names {
		if !strings.Contains(out, n) {
			t.Errorf("missing part row %q in:\n%s", n, out)
		}
	}
	if !strings.Contains(out, "over budget by 200 est. tokens") {
		t.Errorf("missing note in:\n%s", out)
	}
	if got := strings.Count(out, "init context not reported"); got != 2 {
		t.Errorf("'not reported' lines = %d, want 2 in:\n%s", got, out)
	}
}

func TestRunIssueRunsInitContextJSONUnchanged(t *testing.T) {
	runs := []map[string]any{initCtxRun("aaaaaaaa-1111-4222-8333-444444444444",
		[]map[string]any{{"part": "tools", "bytes": 4, "tokens": 1, "budget": 10}})}
	withFlag, err := runIssueRunsForTest(t, runs, "json", true, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	without, err := runIssueRunsForTest(t, runs, "json", false, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if withFlag != without {
		t.Errorf("--output json differs with --init-context:\n%s\nvs\n%s", withFlag, without)
	}
}

func TestRunIssueRunsInitContextRejectsSiblings(t *testing.T) {
	_, err := runIssueRunsForTest(t, nil, "table", true, true)
	if err == nil || !strings.Contains(err.Error(), "--siblings") {
		t.Fatalf("err = %v, want --siblings conflict", err)
	}
}

func TestRunIssueRunsInitContextStripsTerminalControls(t *testing.T) {
	parts := []map[string]any{{
		"part":   "evil\x1b]52;c;ZXZpbA==\x07name\x1b[31m",
		"tokens": 10,
		"budget": "5\x9b0",
		"notes":  []string{"spoof\x1b[2J\x1b]0;title\x1b\\ok\rtail"},
	}}
	out, err := runIssueRunsForTest(t, []map[string]any{
		initCtxRun("aaaaaaaa-1111-4222-8333-444444444444", parts),
	}, "table", true, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, r := range out {
		if r < 0x20 && r != '\n' || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("control char %U in output:\n%q", r, out)
		}
	}
	if !strings.Contains(out, "evil") || !strings.Contains(out, "spoof") {
		t.Errorf("printable text lost in:\n%s", out)
	}
}
