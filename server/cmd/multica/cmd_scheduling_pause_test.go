package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const schedulingWorkspaceID = "11111111-1111-1111-1111-111111111111"

func newSchedulingTestCmd(output string) *cobra.Command {
	cmd := &cobra.Command{Use: "pause"}
	cmd.Flags().String("workspace-id", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("server-url", "", "")
	cmd.Flags().String("output", output, "")
	return cmd
}

func TestRunSchedulingPauseResumeRequests(t *testing.T) {
	tests := []struct {
		name     string
		run      func(*cobra.Command, []string) error
		args     []string
		wantPath string
		wantText string
	}{
		{"agent pause", runAgentPause, []string{"agent-1"}, "/api/agents/agent-1/pause", "Agent scheduling paused: Mika (agent-1)"},
		{"agent resume", runAgentResume, []string{"agent-1"}, "/api/agents/agent-1/resume", "Agent scheduling resumed: Mika (agent-1)"},
		{"workspace pause", runWorkspacePause, []string{schedulingWorkspaceID}, "/api/workspaces/" + schedulingWorkspaceID + "/pause", "Workspace scheduling paused: Cheese (" + schedulingWorkspaceID + ")"},
		{"workspace resume", runWorkspaceResume, []string{schedulingWorkspaceID}, "/api/workspaces/" + schedulingWorkspaceID + "/resume", "Workspace scheduling resumed: Cheese (" + schedulingWorkspaceID + ")"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var gotMethod, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				name := "Mika"
				id := "agent-1"
				if strings.HasPrefix(r.URL.Path, "/api/workspaces/") {
					name, id = "Cheese", schedulingWorkspaceID
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": id, "name": name, "scheduling_paused_at": "2026-11-02T00:00:00Z",
				})
			}))
			defer srv.Close()
			setCLITestServerEnv(t, srv.URL)

			out, err := captureStdout(t, func() error { return tc.run(newSchedulingTestCmd("json"), tc.args) })
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if gotMethod != http.MethodPost || gotPath != tc.wantPath {
				t.Errorf("request = %s %s, want POST %s", gotMethod, gotPath, tc.wantPath)
			}
			if !strings.Contains(out, `"scheduling_paused_at"`) {
				t.Errorf("JSON output missing scheduling_paused_at: %s", out)
			}

			out, err = captureStdout(t, func() error { return tc.run(newSchedulingTestCmd("table"), tc.args) })
			if err != nil {
				t.Fatalf("run table: %v", err)
			}
			if !strings.Contains(out, tc.wantText) {
				t.Errorf("table output %q missing %q", out, tc.wantText)
			}
		})
	}
}

func TestRunSchedulingPauseForbiddenIsClearError(t *testing.T) {
	tests := []struct {
		name string
		run  func(*cobra.Command, []string) error
		args []string
	}{
		{"agent", runAgentPause, []string{"agent-1"}},
		{"workspace", runWorkspaceResume, []string{schedulingWorkspaceID}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"only a person can pause or resume scheduling"}`)
			}))
			defer srv.Close()
			setCLITestServerEnv(t, srv.URL)

			err := tc.run(newSchedulingTestCmd("json"), tc.args)
			if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "only a person can pause or resume scheduling") {
				t.Fatalf("err = %v, want 403 with the server message", err)
			}
		})
	}
}

func TestRunWorkspacePauseResolvesSlugAndDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet && r.URL.Path == "/api/workspaces" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": schedulingWorkspaceID, "name": "Cheese", "slug": "cheese"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": schedulingWorkspaceID, "name": "Cheese"})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", schedulingWorkspaceID)

	if _, err := captureStdout(t, func() error { return runWorkspacePause(newSchedulingTestCmd("json"), []string{"cheese"}) }); err != nil {
		t.Fatalf("by slug: %v", err)
	}
	if _, err := captureStdout(t, func() error { return runWorkspaceResume(newSchedulingTestCmd("json"), nil) }); err != nil {
		t.Fatalf("default workspace: %v", err)
	}
	want := []string{"GET /api/workspaces", "POST /api/workspaces/" + schedulingWorkspaceID + "/pause", "POST /api/workspaces/" + schedulingWorkspaceID + "/resume"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Errorf("requests = %v, want %v", paths, want)
	}
}

func TestSchedulingPausedStateShownInGetAndList(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paused := map[string]any{"scheduling_paused_at": "2026-11-02T00:00:00Z"}
		switch r.URL.Path {
		case "/api/agents/agent-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "agent-1", "name": "Mika", "scheduling_paused_at": paused["scheduling_paused_at"]})
		case "/api/agents":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "agent-1", "name": "Mika", "scheduling_paused_at": paused["scheduling_paused_at"]},
				{"id": "agent-2", "name": "Sol"},
			})
		case "/api/workspaces/" + schedulingWorkspaceID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": schedulingWorkspaceID, "name": "Cheese", "slug": "cheese", "scheduling_paused_at": paused["scheduling_paused_at"]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	getCmd := newSchedulingTestCmd("table")
	out, err := captureStdout(t, func() error { return runAgentGet(getCmd, []string{"agent-1"}) })
	if err != nil || !strings.Contains(out, "PAUSED") || !strings.Contains(out, "2026-11-02T00:00:00Z") {
		t.Errorf("agent get table = %q, err %v; want PAUSED column with the timestamp", out, err)
	}

	listCmd := newSchedulingTestCmd("table")
	listCmd.Flags().Bool("include-archived", false, "")
	out, err = captureStdout(t, func() error { return runAgentList(listCmd, nil) })
	if err != nil || !strings.Contains(out, "PAUSED") || strings.Count(out, "yes") != 1 {
		t.Errorf("agent list table = %q, err %v; want PAUSED column, one paused row", out, err)
	}

	wsCmd := newSchedulingTestCmd("table")
	out, err = captureStdout(t, func() error { return runWorkspaceGet(wsCmd, []string{schedulingWorkspaceID}) })
	if err != nil || !strings.Contains(out, "PAUSED") || !strings.Contains(out, "2026-11-02T00:00:00Z") {
		t.Errorf("workspace get table = %q, err %v; want PAUSED column with the timestamp", out, err)
	}
}
