package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newBreakerTestCmd(t *testing.T, serverURL string) *cobra.Command {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "mat_test-token") // task-scoped prefix also passes the agent-context guard
	cmd := newRuntimeDeleteTestCmd(serverURL)
	_ = cmd.Flags().Set("workspace-id", "ws-123")
	return cmd
}

func TestRunRuntimeBreakerListPrintsBreakers(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		w.Write([]byte(`{"breakers":[{"provider":"openai","state":"open","failure_count":3,"opened_at":"2026-11-07T05:00:00Z","next_probe_at":"2026-11-07T05:05:00Z"}]}`))
	}))
	defer srv.Close()

	out, err := captureRuntimeStdout(t, func() error { return runRuntimeBreakerList(newBreakerTestCmd(t, srv.URL), nil) })
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "GET /api/workspaces/ws-123/provider-breakers" {
		t.Fatalf("request = %q", gotPath)
	}
	for _, want := range []string{"openai", "open", "3", "2026-11-07T05:05:00Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunRuntimeBreakerResetPostsToProvider(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		w.Write([]byte(`{"provider":"openai","previous_state":"open","state":"closed"}`))
	}))
	defer srv.Close()

	out, err := captureRuntimeStdout(t, func() error { return runRuntimeBreakerReset(newBreakerTestCmd(t, srv.URL), []string{"openai"}) })
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /api/workspaces/ws-123/provider-breakers/openai/reset" {
		t.Fatalf("request = %q", gotPath)
	}
	if !strings.Contains(out, "openai") || !strings.Contains(out, "closed") {
		t.Fatalf("output = %q, want the provider and its new state", out)
	}
}
