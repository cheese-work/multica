package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
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
	cmd.Flags().String("expected-before-digest", "", "")
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

func TestResolveSquadInstructionsRejectsInvalidUTF8Inline(t *testing.T) {
	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions", string([]byte{0xff, 0xfe}))

	_, _, err := resolveSquadInstructions(cmd)
	if err == nil || !strings.Contains(err.Error(), "valid UTF-8") {
		t.Fatalf("error = %v, want invalid UTF-8 error", err)
	}
}

func TestRunSquadUpdateDigestModeRejectsInvalidUTF8InlineWithoutHTTPCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions", string([]byte{0xff, 0xfe}))
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)

	err := runSquadUpdate(cmd, []string{"squad-123"})
	if err == nil || !strings.Contains(err.Error(), "valid UTF-8") {
		t.Fatalf("error = %v, want invalid UTF-8 error", err)
	}
	if called {
		t.Fatal("invalid inline UTF-8 must be rejected client-side without an HTTP call")
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

// ── CHE-789: digest-mode conditional updates ───────────────────────────────

func TestBuildSquadUpdateDigestBody(t *testing.T) {
	t.Run("builds single-field body with instructions and digest", func(t *testing.T) {
		cmd := newSquadUpdateTestCmd()
		_ = cmd.Flags().Set("instructions", "hermes-applied instructions")
		_ = cmd.Flags().Set("expected-before-digest", testDigestHex)

		body, err := buildSquadUpdateDigestBody(cmd)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(body) != 2 {
			t.Fatalf("body = %v, want exactly 2 keys (instructions, expected_before_digest)", body)
		}
		if body["instructions"] != "hermes-applied instructions" {
			t.Errorf("instructions = %v, want hermes-applied instructions", body["instructions"])
		}
		if body["expected_before_digest"] != testDigestHex {
			t.Errorf("expected_before_digest = %v, want %v", body["expected_before_digest"], testDigestHex)
		}
	})

	t.Run("malformed digest rejected client-side", func(t *testing.T) {
		cmd := newSquadUpdateTestCmd()
		_ = cmd.Flags().Set("instructions", "new instructions")
		_ = cmd.Flags().Set("expected-before-digest", "short")

		_, err := buildSquadUpdateDigestBody(cmd)
		if err == nil || !strings.Contains(err.Error(), "64 hex characters") {
			t.Fatalf("err = %v, want malformed digest rejection", err)
		}
	})

	t.Run("missing instructions is rejected", func(t *testing.T) {
		cmd := newSquadUpdateTestCmd()
		_ = cmd.Flags().Set("expected-before-digest", testDigestHex)

		_, err := buildSquadUpdateDigestBody(cmd)
		if err == nil || !strings.Contains(err.Error(), "requires the new instructions") {
			t.Fatalf("err = %v, want missing-instructions rejection", err)
		}
	})

	t.Run("conflicting flag rejected client-side", func(t *testing.T) {
		cmd := newSquadUpdateTestCmd()
		_ = cmd.Flags().Set("instructions", "new instructions")
		_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
		_ = cmd.Flags().Set("name", "conflicting name")

		_, err := buildSquadUpdateDigestBody(cmd)
		if err == nil || !strings.Contains(err.Error(), "--name") {
			t.Fatalf("err = %v, want conflicting --name rejection", err)
		}
	})

	t.Run("leader flag conflicts too", func(t *testing.T) {
		cmd := newSquadUpdateTestCmd()
		_ = cmd.Flags().Set("instructions", "new instructions")
		_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
		_ = cmd.Flags().Set("leader", "some-agent")

		_, err := buildSquadUpdateDigestBody(cmd)
		if err == nil || !strings.Contains(err.Error(), "--leader") {
			t.Fatalf("err = %v, want conflicting --leader rejection", err)
		}
	})
}

func squadUpdateDigestTestServer(t *testing.T, body *map[string]any) *httptest.Server {
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"squad":         map[string]any{"id": "squad-123", "instructions": (*body)["instructions"]},
			"before_digest": "before123",
			"after_digest":  "after456",
		})
	}))
}

func TestRunSquadUpdateDigestModeBuildsSingleFieldBody(t *testing.T) {
	var body map[string]any
	srv := squadUpdateDigestTestServer(t, &body)
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions", "hermes-applied instructions")
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
	_ = cmd.Flags().Set("output", "json")

	out, err := captureStdout(t, func() error { return runSquadUpdate(cmd, []string{"squad-123"}) })
	if err != nil {
		t.Fatalf("runSquadUpdate: %v", err)
	}
	if len(body) != 2 {
		t.Fatalf("request body = %v, want exactly instructions + expected_before_digest", body)
	}
	if body["instructions"] != "hermes-applied instructions" {
		t.Errorf("instructions = %v, want hermes-applied instructions", body["instructions"])
	}
	if body["expected_before_digest"] != testDigestHex {
		t.Errorf("expected_before_digest = %v, want %v", body["expected_before_digest"], testDigestHex)
	}

	var printed map[string]any
	if err := json.Unmarshal([]byte(out), &printed); err != nil {
		t.Fatalf("decode stdout JSON %q: %v", out, err)
	}
	if printed["before_digest"] != "before123" || printed["after_digest"] != "after456" {
		t.Errorf("printed digests = %v, want before123/after456", printed)
	}
}

func TestRunSquadUpdateDigestModeRejectsMalformedDigestWithoutHTTPCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions", "new instructions")
	_ = cmd.Flags().Set("expected-before-digest", "not-64-hex")

	err := runSquadUpdate(cmd, []string{"squad-123"})
	if err == nil || !strings.Contains(err.Error(), "64 hex characters") {
		t.Fatalf("err = %v, want malformed digest rejection", err)
	}
	if called {
		t.Fatal("malformed digest must be rejected client-side without an HTTP call")
	}
}

func TestRunSquadUpdateDigestModeRejectsConflictingFlagWithoutHTTPCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions", "new instructions")
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
	_ = cmd.Flags().Set("avatar-url", "https://example.test/avatar.png")

	err := runSquadUpdate(cmd, []string{"squad-123"})
	if err == nil || !strings.Contains(err.Error(), "--avatar-url") {
		t.Fatalf("err = %v, want conflicting --avatar-url rejection", err)
	}
	if called {
		t.Fatal("conflicting flag combination must be rejected client-side without an HTTP call")
	}
}

func TestRunSquadUpdateDigestModeSurfaces403WithoutRetry(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "actor is not permitted to write this field"})
	}))
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions", "attempted instructions")
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)

	err := runSquadUpdate(cmd, []string{"squad-123"})
	if err == nil {
		t.Fatal("expected error surfaced from 403 response")
	}
	var httpErr *cli.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v, want *cli.HTTPError", err)
	}
	if httpErr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", httpErr.StatusCode)
	}
	if callCount != 1 {
		t.Errorf("server called %d times, want exactly 1 (no retry)", callCount)
	}
}

func TestRunSquadUpdateDigestModeSurfaces409WithoutRetry(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "expected_before_digest is stale"})
	}))
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions", "attempted instructions")
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)

	err := runSquadUpdate(cmd, []string{"squad-123"})
	if err == nil {
		t.Fatal("expected error surfaced from 409 response")
	}
	var httpErr *cli.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v, want *cli.HTTPError", err)
	}
	if httpErr.StatusCode != http.StatusConflict {
		t.Errorf("StatusCode = %d, want 409", httpErr.StatusCode)
	}
	if callCount != 1 {
		t.Errorf("server called %d times, want exactly 1 (no retry)", callCount)
	}
}

func TestRunSquadUpdateNonDigestModeUnchanged(t *testing.T) {
	var body map[string]any
	srv := squadUpdateTestServer(t, &body)
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newSquadUpdateTestCmd()
	_ = cmd.Flags().Set("instructions", "plain update")
	_ = cmd.Flags().Set("output", "json")

	if _, err := captureStdout(t, func() error { return runSquadUpdate(cmd, []string{"squad-123"}) }); err != nil {
		t.Fatalf("runSquadUpdate: %v", err)
	}
	if body["instructions"] != "plain update" {
		t.Errorf("instructions = %v, want plain update", body["instructions"])
	}
	if _, present := body["expected_before_digest"]; present {
		t.Errorf("expected_before_digest must not appear when --expected-before-digest was not set, got %v", body)
	}
}
