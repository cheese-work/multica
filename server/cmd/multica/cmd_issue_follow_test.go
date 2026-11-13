package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
)

func followTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "follow"}
	c.Flags().String("chat", "", "")
	c.Flags().String("output", "json", "")
	return c
}

func TestIssueFollowCommandsCallTheProgressFollowAPI(t *testing.T) {
	var method, path, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"follows": []any{}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	base := "/api/issues/" + testIssueUUID + "/progress-follow"

	cases := []struct {
		name, method, suffix string
		chat                 string
		wantMethod, wantPath string
		wantQuery            string
	}{
		{"agent follow sends no lane", "POST", "", "", "POST", base, ""},
		{"member follow names the chat", "POST", "", "11111111-1111-1111-1111-111111111111", "POST", base, "chat_session_id=11111111-1111-1111-1111-111111111111"},
		{"mute", "POST", "/mute", "11111111-1111-1111-1111-111111111111", "POST", base + "/mute", "chat_session_id=11111111-1111-1111-1111-111111111111"},
		{"unfollow", "DELETE", "", "11111111-1111-1111-1111-111111111111", "DELETE", base, "chat_session_id=11111111-1111-1111-1111-111111111111"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			method, path, query = "", "", ""
			cmd := followTestCmd()
			if c.chat != "" {
				_ = cmd.Flags().Set("chat", c.chat)
			}
			if err := runIssueFollowAction(c.method, c.suffix)(cmd, []string{testIssueUUID}); err != nil {
				t.Fatal(err)
			}
			if method != c.wantMethod || path != c.wantPath || query != c.wantQuery {
				t.Fatalf("sent %s %s?%s, want %s %s?%s", method, path, query, c.wantMethod, c.wantPath, c.wantQuery)
			}
		})
	}

	t.Run("follows list", func(t *testing.T) {
		cmd := followTestCmd()
		_ = cmd.Flags().Set("chat", "11111111-1111-1111-1111-111111111111")
		if err := runIssueFollows(cmd, nil); err != nil {
			t.Fatal(err)
		}
		if method != "GET" || path != "/api/dm-progress/follows" || query != "chat_session_id=11111111-1111-1111-1111-111111111111" {
			t.Fatalf("sent %s %s?%s", method, path, query)
		}
	})
}

func TestIssueUnfollowScopeReachesTheAPI(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := issueUnfollowCmd
	t.Cleanup(func() {
		_ = cmd.Flags().Set("scope", "")
		_ = cmd.Flags().Set("chat", "")
	})
	for flag, value := range map[string]string{"chat": "11111111-1111-1111-1111-111111111111", "scope": "root"} {
		if err := cmd.Flags().Set(flag, value); err != nil {
			t.Fatalf("unfollow has no --%s flag: %v", flag, err)
		}
	}
	if err := runIssueFollowAction("DELETE", "")(cmd, []string{testIssueUUID}); err != nil {
		t.Fatal(err)
	}
	if query != "chat_session_id=11111111-1111-1111-1111-111111111111&scope=root" {
		t.Fatalf("sent query %q, want the chat and scope=root", query)
	}
}
