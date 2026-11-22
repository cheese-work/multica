package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func guardLogServer(t *testing.T) *[]string {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.Query().Encode())
		switch r.URL.Path {
		case "/api/guard/decisions":
			_ = json.NewEncoder(w).Encode(map[string]any{"decisions": []map[string]any{
				{"time": "2026-11-13T15:41:00Z", "plugin": "pr-title-tag", "action": "fixed", "runtime": "claude", "issue": "CHE-1", "line": "title tag [merge] added"},
			}})
		case "/api/guard/stats":
			_ = json.NewEncoder(w).Encode(map[string]any{"days": 7, "plugins": []map[string]any{
				{"plugin": "pr-title-tag", "fixed": 42, "refused": 3, "errors": 0},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	return &seen
}

func guardLogTestCmd(flags map[string]string) *cobra.Command {
	c := &cobra.Command{Use: "log"}
	c.Flags().String("plugin", "", "")
	c.Flags().String("action", "", "")
	c.Flags().String("since", "7d", "")
	c.Flags().Int("limit", 50, "")
	c.Flags().Int("days", 7, "")
	c.Flags().String("output", "json", "")
	for k, v := range flags {
		_ = c.Flags().Set(k, v)
	}
	return c
}

func TestGuardLogAsksTheServerForTheFilteredWindow(t *testing.T) {
	seen := guardLogServer(t)
	out, err := captureStdout(t, func() error {
		return runGuardLog(guardLogTestCmd(map[string]string{"plugin": "pr-title-tag", "since": "24h", "limit": "5"}), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 {
		t.Fatalf("seen %v", *seen)
	}
	want := "GET /api/guard/decisions?limit=5&plugin=pr-title-tag&since=24h"
	if (*seen)[0] != want {
		t.Fatalf("request %q, want %q", (*seen)[0], want)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0]["issue"] != "CHE-1" {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestGuardStatsPrintsOneRowPerPlugin(t *testing.T) {
	seen := guardLogServer(t)
	out, err := captureStdout(t, func() error { return runGuardStats(guardLogTestCmd(map[string]string{"days": "7"}), nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix((*seen)[0], "GET /api/guard/stats?days=7") {
		t.Fatalf("request %q", (*seen)[0])
	}
	var rows []struct {
		Plugin                 string
		Fixed, Refused, Errors int
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0].Fixed != 42 || rows[0].Refused != 3 {
		t.Fatalf("out %q err %v", out, err)
	}
}
