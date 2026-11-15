package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssueHumanInChargeCommand(t *testing.T) {
	for name, tc := range map[string]struct {
		response string
		want     string
	}{
		"member": {`{"human":{"user_id":"u-1","name":"Cheese","source":"creator"}}`, "u-1\tCheese\tcreator"},
		"none":   {`{"human":null,"reason":"nobody eligible"}`, "none: nobody eligible"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/issues/CHE-1":
					json.NewEncoder(w).Encode(map[string]string{"id": testIssueUUID, "identifier": "CHE-1"})
				case "/api/issues/" + testIssueUUID + "/human-in-charge":
					w.Write([]byte(tc.response))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			t.Setenv("MULTICA_SERVER_URL", srv.URL)
			t.Setenv("MULTICA_TOKEN", "mat_test-token")
			t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			cmd, _, err := rootCmd.Find([]string{"issue", "human-in-charge"})
			if err != nil || cmd.Name() != "human-in-charge" {
				t.Fatal("issue human-in-charge is unavailable")
			}
			out, err := captureStdout(t, func() error { return cmd.RunE(cmd, []string{"CHE-1"}) })
			if err != nil || strings.TrimSpace(out) != tc.want {
				t.Fatalf("output %q err %v, want %q", out, err, tc.want)
			}
		})
	}
}
