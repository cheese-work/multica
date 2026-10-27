package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newKillSwitchTestCmd(output string) *cobra.Command {
	cmd := &cobra.Command{Use: "kill-switch"}
	cmd.Flags().String("scope", "*", "")
	cmd.Flags().String("output", output, "")
	return cmd
}

func TestGitHubKillSwitchStatusReadsWithoutWriting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/github/dispatch-halt" {
			t.Fatalf("status sent %s %s, want GET /api/github/dispatch-halt", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"halts": []map[string]any{{"scope": "acme/widget", "halted_by": nil, "halted_at": "2026-10-27T11:00:00Z"}}})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	out, err := captureStdout(t, func() error { return runGitHubKillSwitchStatus(newKillSwitchTestCmd("table"), nil) })
	if err != nil || !strings.Contains(out, "acme/widget") {
		t.Fatalf("status = %q, %v; want the halted scope", out, err)
	}
}

func TestGitHubKillSwitchOnAndOffSendScopeAndState(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/github/dispatch-halt" {
			t.Fatalf("request = %s %s, want PUT /api/github/dispatch-halt", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"halts": []any{}, "runs_revoked": 0})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	on := newKillSwitchTestCmd("json")
	_ = on.Flags().Set("scope", "acme/widget")
	if _, err := captureStdout(t, func() error { return runGitHubKillSwitch(true)(on, nil) }); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return runGitHubKillSwitch(false)(newKillSwitchTestCmd("table"), nil) })
	if err != nil || !strings.Contains(out, "kill switch: off") {
		t.Fatalf("off output = %q, %v", out, err)
	}
	if len(bodies) != 2 || bodies[0]["scope"] != "acme/widget" || bodies[0]["halted"] != true || bodies[1]["scope"] != "*" || bodies[1]["halted"] != false {
		t.Fatalf("bodies = %v, want on acme/widget then off *", bodies)
	}
}
