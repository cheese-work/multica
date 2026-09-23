package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

const provenanceTestWorkspace = "11111111-1111-1111-1111-111111111111"

func newProvenanceExportTestCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "export"}
	addProvenanceExportFlags(cmd)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return cmd
}

func provenanceTestServer(t *testing.T, calls *atomic.Int32, handle func(w http.ResponseWriter, r *http.Request)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if handle == nil {
			t.Errorf("unexpected HTTP call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	clearAmbientDaemonTaskEnv(t)
	setCLITestServerEnv(t, srv.URL)
}

func TestProvenanceExportFailsClosedWithoutHTTP(t *testing.T) {
	overCap := []string{"--workspace", provenanceTestWorkspace, "--cutoff", "2026-09-01T00:00:00Z"}
	for i := 0; i <= provenanceMaxSources; i++ {
		overCap = append(overCap, "--issue", fmt.Sprintf("MUL-%d", i+1))
	}
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing workspace", []string{"--issue", "MUL-1", "--cutoff", "2026-09-01T00:00:00Z"}, "--workspace is required"},
		{"two workspaces", []string{"--workspace", provenanceTestWorkspace, "--workspace", provenanceTestWorkspace, "--issue", "MUL-1", "--cutoff", "2026-09-01T00:00:00Z"}, "exactly once"},
		{"blank workspace", []string{"--workspace", " ", "--issue", "MUL-1", "--cutoff", "2026-09-01T00:00:00Z"}, "must not be empty"},
		{"workspace not a uuid", []string{"--workspace", "acme-prod", "--issue", "MUL-1", "--cutoff", "2026-09-01T00:00:00Z"}, "--workspace must be a valid UUID"},
		{"over source cap", overCap, "at most 256 per export"},
		{"missing cutoff", []string{"--workspace", provenanceTestWorkspace, "--issue", "MUL-1"}, "--cutoff is required"},
		{"bad cutoff", []string{"--workspace", provenanceTestWorkspace, "--issue", "MUL-1", "--cutoff", "2026-09-01"}, "--cutoff must be RFC3339"},
		{"no sources", []string{"--workspace", provenanceTestWorkspace, "--cutoff", "2026-09-01T00:00:00Z"}, "at least one --issue or --thread"},
		{"bad output", []string{"--workspace", provenanceTestWorkspace, "--issue", "MUL-1", "--cutoff", "2026-09-01T00:00:00Z", "--output", "yaml"}, "--output must be json or table"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			provenanceTestServer(t, &calls, nil)
			// The ambient workspace must never stand in for --workspace.
			t.Setenv("MULTICA_WORKSPACE_ID", provenanceTestWorkspace)

			err := runProvenanceExport(newProvenanceExportTestCmd(t, tc.args...), nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "provenance export: ") {
				t.Fatalf("err = %q, want provenance export prefix", err)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("HTTP calls = %d, want 0", got)
			}
		})
	}
}

func TestProvenanceExportErrorsNeverEchoFlagValues(t *testing.T) {
	secret := strings.Join([]string{"gh", "p_", strings.Repeat("Q7w6", 10)}, "")
	// A long value would expose any interpolation through the length bound
	// even if it stopped matching the secret substring.
	long := secret + strings.Repeat("x", 4096)
	const maxErrLen = 200
	base := func(workspace, cutoff, output string) []string {
		return []string{"--workspace", workspace, "--issue", "MUL-1", "--cutoff", cutoff, "--output", output}
	}
	cases := []struct {
		name string
		args []string
	}{
		{"workspace", base(secret, "2026-09-01T00:00:00Z", "json")},
		{"long workspace", base(long, "2026-09-01T00:00:00Z", "json")},
		{"cutoff", base(provenanceTestWorkspace, secret, "json")},
		{"long cutoff", base(provenanceTestWorkspace, long, "json")},
		{"output", base(provenanceTestWorkspace, "2026-09-01T00:00:00Z", secret)},
		{"long output", base(provenanceTestWorkspace, "2026-09-01T00:00:00Z", long)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			provenanceTestServer(t, &calls, nil)
			err := runProvenanceExport(newProvenanceExportTestCmd(t, tc.args...), nil)
			if err == nil {
				t.Fatal("want an error")
			}
			if msg := err.Error(); strings.Contains(msg, secret) || len(msg) >= maxErrLen {
				t.Fatalf("error echoes input or is unbounded (len %d): %q", len(msg), msg)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("HTTP calls = %d, want 0", got)
			}
		})
	}
}

func TestProvenanceExportPostsExplicitScope(t *testing.T) {
	var calls atomic.Int32
	var body map[string]any
	var wsHeader string
	provenanceTestServer(t, &calls, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/provenance/export" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		wsHeader = r.Header.Get("X-Workspace-ID")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"export_id":        "exp-1",
			"request_digest":   "req-digest",
			"manifest_digest":  "man-digest",
			"cutoff":           "2026-09-01T00:00:00Z",
			"generated_at":     "2026-09-02T00:00:00Z",
			"redaction_policy": "redact.Text:v1",
			"records":          []any{map[string]any{"kind": "issue", "id": "i1"}},
			"exclusions":       []any{},
		})
	})

	cmd := newProvenanceExportTestCmd(t,
		"--workspace", provenanceTestWorkspace,
		"--issue", "MUL-1", "--issue", "MUL-2",
		"--thread", "22222222-2222-2222-2222-222222222222",
		"--cutoff", "2026-09-01T00:00:00Z")
	out, err := captureStdout(t, func() error { return runProvenanceExport(cmd, nil) })
	if err != nil {
		t.Fatalf("runProvenanceExport: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls.Load())
	}
	if wsHeader != provenanceTestWorkspace {
		t.Fatalf("X-Workspace-ID = %q, want explicit --workspace (not ambient ws-1)", wsHeader)
	}
	if body["workspace_id"] != provenanceTestWorkspace || body["cutoff"] != "2026-09-01T00:00:00Z" {
		t.Fatalf("body = %v", body)
	}
	if issues, _ := body["issues"].([]any); len(issues) != 2 || issues[0] != "MUL-1" {
		t.Fatalf("issues = %v", body["issues"])
	}
	if threads, _ := body["threads"].([]any); len(threads) != 1 {
		t.Fatalf("threads = %v", body["threads"])
	}
	if _, ok := body["chat_sessions"]; ok {
		t.Fatalf("body must not carry chat sources: %v", body)
	}
	if !strings.Contains(out, `"manifest_digest": "man-digest"`) || !strings.Contains(out, `"id": "i1"`) {
		t.Fatalf("stdout = %s", out)
	}
}

func TestProvenanceExportTableSummary(t *testing.T) {
	var calls atomic.Int32
	provenanceTestServer(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"export_id":       "exp-9",
			"manifest_digest": "abc123",
			"records":         []any{map[string]any{}, map[string]any{}},
			"exclusions":      []any{map[string]any{"reason": "out_of_cutoff"}},
		})
	})

	cmd := newProvenanceExportTestCmd(t,
		"--workspace", provenanceTestWorkspace, "--issue", "MUL-1",
		"--cutoff", "2026-09-01T00:00:00Z", "--output", "table")
	out, err := captureStdout(t, func() error { return runProvenanceExport(cmd, nil) })
	if err != nil {
		t.Fatalf("runProvenanceExport: %v", err)
	}
	for _, want := range []string{"exp-9", "abc123", "records", "2", "exclusions", "1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestProvenanceExportWrapsServerError(t *testing.T) {
	var calls atomic.Int32
	provenanceTestServer(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"insufficient permissions"}`))
	})
	cmd := newProvenanceExportTestCmd(t,
		"--workspace", provenanceTestWorkspace, "--issue", "MUL-1", "--cutoff", "2026-09-01T00:00:00Z")
	_, err := captureStdout(t, func() error { return runProvenanceExport(cmd, nil) })
	if err == nil || !strings.HasPrefix(err.Error(), "provenance export: ") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}
