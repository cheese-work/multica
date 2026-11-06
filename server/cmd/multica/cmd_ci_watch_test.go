package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

type ciWatchCall struct {
	method, uri string
	body        map[string]any
}

// ciWatchServer records the last request and answers with a small JSON object.
func ciWatchServer(t *testing.T) *ciWatchCall {
	t.Helper()
	got := &ciWatchCall{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.uri, got.body = r.Method, r.RequestURI, nil
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_ = json.NewEncoder(w).Encode(map[string]any{"repo": "acme/widgets", "mode": "off", "events": []any{}})
	}))
	t.Cleanup(server.Close)
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_SERVER_URL", server.URL)
	t.Setenv("MULTICA_TOKEN", "mul_fake-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	return got
}

func runCI(t *testing.T, cmd *cobra.Command, run func(*cobra.Command, []string) error, args ...string) error {
	t.Helper()
	_, err := captureStdout(t, func() error { return run(cmd, args) })
	return err
}

func TestCIWatchGet(t *testing.T) {
	got := ciWatchServer(t)
	if err := runCI(t, newCIWatchGetCmd(), runCIWatchGet, "Acme/Widgets"); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodGet || got.uri != "/api/workspaces/ws-1/ci-watch/repos/Acme%2FWidgets" {
		t.Fatalf("get called %s %s", got.method, got.uri)
	}
}

func TestCIWatchSet(t *testing.T) {
	got := ciWatchServer(t)
	cmd := newCIWatchSetCmd()
	for k, v := range map[string]string{"mode": "act", "branches": "main,release/*", "max-new-issues": "9"} {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := runCI(t, cmd, runCIWatchSet, "acme/widgets"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"mode": "act", "branches": []any{"main", "release/*"}, "max_new_issues_per_day": float64(9)}
	if got.method != http.MethodPut || got.uri != "/api/workspaces/ws-1/ci-watch/repos/acme%2Fwidgets" || !reflect.DeepEqual(got.body, want) {
		t.Fatalf("set called %s %s %v, want body %v", got.method, got.uri, got.body, want)
	}

	// Owner pair and clear flags.
	cmd = newCIWatchSetCmd()
	_ = cmd.Flags().Set("fix-owner-kind", "agent")
	_ = cmd.Flags().Set("fix-owner-id", "11111111-1111-1111-1111-111111111111")
	_ = cmd.Flags().Set("clear-notify-member", "true")
	if err := runCI(t, cmd, runCIWatchSet, "acme/widgets"); err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"fix_owner_kind": "agent", "fix_owner_id": "11111111-1111-1111-1111-111111111111", "notify_member_id": nil}
	if !reflect.DeepEqual(got.body, want) {
		t.Fatalf("owner body = %v, want %v", got.body, want)
	}

	// No flag, or a flag that contradicts its clear twin: refused before any request.
	got.method = ""
	if err := runCI(t, newCIWatchSetCmd(), runCIWatchSet, "acme/widgets"); err == nil {
		t.Fatal("set with no flags must fail")
	}
	cmd = newCIWatchSetCmd()
	_ = cmd.Flags().Set("notify-member", "11111111-1111-1111-1111-111111111111")
	_ = cmd.Flags().Set("clear-notify-member", "true")
	if err := runCI(t, cmd, runCIWatchSet, "acme/widgets"); err == nil {
		t.Fatal("notify-member with clear-notify-member must fail")
	}
	if got.method != "" {
		t.Fatalf("refused set still sent %s", got.method)
	}
}

func TestCIWatchEvents(t *testing.T) {
	got := ciWatchServer(t)
	if err := runCI(t, newCIWatchEventsCmd(), runCIWatchEvents, "acme/widgets"); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodGet || got.uri != "/api/workspaces/ws-1/ci-watch/repos/acme%2Fwidgets/events" {
		t.Fatalf("events called %s %s", got.method, got.uri)
	}
	cmd := newCIWatchEventsCmd()
	_ = cmd.Flags().Set("limit", "5")
	if err := runCI(t, cmd, runCIWatchEvents, "acme/widgets"); err != nil {
		t.Fatal(err)
	}
	if got.uri != "/api/workspaces/ws-1/ci-watch/repos/acme%2Fwidgets/events?limit=5" {
		t.Fatalf("events with limit called %s", got.uri)
	}
}
