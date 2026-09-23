package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newSquadUpdateTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "update"}
	cmd.Flags().String("server-url", "", "")
	cmd.Flags().String("workspace-id", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("name", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().String("instructions", "", "")
	cmd.Flags().Bool("instructions-stdin", false, "")
	cmd.Flags().String("instructions-file", "", "")
	cmd.Flags().String("leader", "", "")
	cmd.Flags().String("avatar-url", "", "")
	cmd.Flags().String("output", "json", "")
	return cmd
}

func squadUpdateTestServer(t *testing.T, body *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/api/squads/squad-123" {
			t.Fatalf("path = %q, want squad update path", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "squad-123",
			"name":         "squad",
			"instructions": (*body)["instructions"],
		})
	}))
}

func setSquadUpdateServerEnv(t *testing.T, serverURL string) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MULTICA_SERVER_URL", serverURL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	t.Setenv("MULTICA_DAEMON_PORT", "")
}

func TestRunSquadUpdateReadsInstructionsFromStdinVerbatim(t *testing.T) {
	var body map[string]any
	srv := squadUpdateTestServer(t, &body)
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	want := "# Heading\n\nCyrillic: проверка\\n\n"
	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions-stdin", "true")
	cmd.SetIn(bytes.NewBufferString(want))
	output, err := captureStdout(t, func() error { return runSquadUpdate(cmd, []string{"squad-123"}) })
	if err != nil {
		t.Fatalf("runSquadUpdate: %v", err)
	}
	if body["instructions"] != want {
		t.Fatalf("request instructions = %q, want %q", body["instructions"], want)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if got["instructions"] != want {
		t.Fatalf("readback instructions = %q, want %q", got["instructions"], want)
	}
}

func TestRunSquadUpdateReadsInstructionsFromFileVerbatim(t *testing.T) {
	var body map[string]any
	srv := squadUpdateTestServer(t, &body)
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	want := "line one\n\nJapanese: 漢字\n"
	path := t.TempDir() + string(os.PathSeparator) + "instructions.md"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatalf("write instructions: %v", err)
	}
	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions-file", path)
	if _, err := captureStdout(t, func() error { return runSquadUpdate(cmd, []string{"squad-123"}) }); err != nil {
		t.Fatalf("runSquadUpdate: %v", err)
	}
	if body["instructions"] != want {
		t.Fatalf("request instructions = %q, want %q", body["instructions"], want)
	}
}

func TestRunSquadUpdateRejectsConflictingInstructionInputs(t *testing.T) {
	setSquadUpdateServerEnv(t, "http://127.0.0.1:1")

	path := t.TempDir() + string(os.PathSeparator) + "instructions.md"
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatalf("write instructions: %v", err)
	}
	for _, tc := range []struct {
		name string
		set  func(*cobra.Command)
	}{
		{name: "inline and stdin", set: func(cmd *cobra.Command) {
			_ = cmd.Flags().Set("instructions", "inline")
			_ = cmd.Flags().Set("instructions-stdin", "true")
		}},
		{name: "inline and file", set: func(cmd *cobra.Command) {
			_ = cmd.Flags().Set("instructions", "inline")
			_ = cmd.Flags().Set("instructions-file", path)
		}},
		{name: "stdin and file", set: func(cmd *cobra.Command) {
			_ = cmd.Flags().Set("instructions-stdin", "true")
			_ = cmd.Flags().Set("instructions-file", path)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newSquadUpdateTestCmd()
			tc.set(cmd)
			err := runSquadUpdate(cmd, []string{"squad-123"})
			if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
				t.Fatalf("error = %v, want mutually exclusive input error", err)
			}
		})
	}
}

func TestResolveSquadInstructionsRejectsInvalidUTF8(t *testing.T) {
	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions-stdin", "true")
	cmd.SetIn(bytes.NewReader([]byte{0xff}))

	_, _, err := resolveSquadInstructions(cmd)
	if err == nil || !strings.Contains(err.Error(), "valid UTF-8") {
		t.Fatalf("error = %v, want invalid UTF-8 error", err)
	}
}

func newSquadMemberSetRoleTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "set-role"}
	cmd.Flags().String("server-url", "", "")
	cmd.Flags().String("workspace-id", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("member-id", "", "")
	cmd.Flags().String("member-type", "agent", "")
	cmd.Flags().String("role", "", "")
	cmd.Flags().String("output", "json", "")
	return cmd
}

func TestRunSquadMemberSetRolePatchesRole(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "workspace-123")

	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if r.Header.Get("X-Workspace-ID") != "workspace-123" {
			t.Fatalf("X-Workspace-ID = %q, want workspace-123", r.Header.Get("X-Workspace-ID"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"squad_id":    "squad-123",
			"member_id":   "member-456",
			"member_type": "agent",
			"role":        "reviewer",
		})
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)

	cmd := newSquadMemberSetRoleTestCmd()
	_ = cmd.Flags().Set("member-id", "member-456")
	_ = cmd.Flags().Set("member-type", "agent")
	_ = cmd.Flags().Set("role", "reviewer")
	_ = cmd.Flags().Set("output", "json")

	if err := runSquadMemberSetRole(cmd, []string{"squad-123"}); err != nil {
		t.Fatalf("runSquadMemberSetRole: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Fatalf("method = %s, want PATCH", gotMethod)
	}
	if gotPath != "/api/squads/squad-123/members/role" {
		t.Fatalf("path = %q, want /api/squads/squad-123/members/role", gotPath)
	}
	wantBody := map[string]any{"member_id": "member-456", "member_type": "agent", "role": "reviewer"}
	for k, want := range wantBody {
		if gotBody[k] != want {
			t.Fatalf("body[%s] = %v, want %v (full body: %#v)", k, gotBody[k], want, gotBody)
		}
	}
}

func TestRunSquadMemberSetRoleValidatesRequiredFlags(t *testing.T) {
	cmd := newSquadMemberSetRoleTestCmd()
	if err := runSquadMemberSetRole(cmd, []string{"squad-123"}); err == nil {
		t.Fatal("expected missing --member-id error")
	}

	cmd = newSquadMemberSetRoleTestCmd()
	_ = cmd.Flags().Set("member-id", "member-456")
	_ = cmd.Flags().Set("member-type", "invalid")
	if err := runSquadMemberSetRole(cmd, []string{"squad-123"}); err == nil {
		t.Fatal("expected invalid --member-type error")
	}

	cmd = newSquadMemberSetRoleTestCmd()
	_ = cmd.Flags().Set("member-id", "member-456")
	if err := runSquadMemberSetRole(cmd, []string{"squad-123"}); err == nil {
		t.Fatal("expected missing --role error")
	}
}
