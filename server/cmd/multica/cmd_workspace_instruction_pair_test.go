package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newWorkspaceApplyInstructionPairTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "apply-instruction-pair"}
	cmd.Flags().String("server-url", "", "")
	cmd.Flags().String("workspace-id", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("squad-id", "", "")
	cmd.Flags().String("context-file", "", "")
	cmd.Flags().String("instructions-file", "", "")
	cmd.Flags().String("expected-context-before-digest", "", "")
	cmd.Flags().String("expected-instructions-before-digest", "", "")
	cmd.Flags().String("expected-context-after-digest", "", "")
	cmd.Flags().String("expected-instructions-after-digest", "", "")
	cmd.Flags().String("output", "json", "")
	return cmd
}

func instructionPairTestDigest(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func setInstructionPairTestFlags(t *testing.T, cmd *cobra.Command, contextPath, instructionsPath, contextText, instructionsText string) {
	t.Helper()
	values := map[string]string{
		"squad-id":                            "22222222-2222-2222-2222-222222222222",
		"context-file":                        contextPath,
		"instructions-file":                   instructionsPath,
		"expected-context-before-digest":      instructionPairTestDigest("old context"),
		"expected-instructions-before-digest": instructionPairTestDigest("old instructions"),
		"expected-context-after-digest":       instructionPairTestDigest(contextText),
		"expected-instructions-after-digest":  instructionPairTestDigest(instructionsText),
	}
	for name, value := range values {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
	}
}

func TestRunWorkspaceApplyInstructionPairUsesOnePublicRequestAndPreservesBytes(t *testing.T) {
	contextText := "context\r\nwith unicode 漢字 and \\\\n\n"
	instructionsText := "instructions\nwith literal \\\\n and $(not-shell)\n\n"
	contextPath := filepath.Join(t.TempDir(), "context.md")
	instructionsPath := filepath.Join(t.TempDir(), "instructions.md")
	if err := os.WriteFile(contextPath, []byte(contextText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instructionsPath, []byte(instructionsText), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls int
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPut || r.URL.Path != "/api/workspaces/11111111-1111-1111-1111-111111111111/instruction-pair" {
			t.Errorf("request = %s %s, want single instruction-pair PUT", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspace_id":               "11111111-1111-1111-1111-111111111111",
			"context_before_digest":      instructionPairTestDigest("old context"),
			"context_after_digest":       instructionPairTestDigest(contextText),
			"squad_id":                   "22222222-2222-2222-2222-222222222222",
			"instructions_before_digest": instructionPairTestDigest("old instructions"),
			"instructions_after_digest":  instructionPairTestDigest(instructionsText),
		})
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	cmd := newWorkspaceApplyInstructionPairTestCmd()
	setInstructionPairTestFlags(t, cmd, contextPath, instructionsPath, contextText, instructionsText)

	if err := runWorkspaceApplyInstructionPair(cmd, []string{"11111111-1111-1111-1111-111111111111"}); err != nil {
		t.Fatalf("runWorkspaceApplyInstructionPair: %v", err)
	}
	if calls != 1 {
		t.Fatalf("HTTP calls = %d, want exactly one paired request", calls)
	}
	if len(received) != 5 {
		t.Fatalf("request fields = %v, want exactly five paired-CAS fields", received)
	}
	if received["context"] != contextText || received["instructions"] != instructionsText {
		t.Fatalf("request lost candidate bytes: %#v", received)
	}
	if received["squad_id"] != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("squad_id = %v", received["squad_id"])
	}
}

func TestRunWorkspaceApplyInstructionPairRejectsChangedCandidateBeforeRequest(t *testing.T) {
	contextPath := filepath.Join(t.TempDir(), "context.md")
	instructionsPath := filepath.Join(t.TempDir(), "instructions.md")
	if err := os.WriteFile(contextPath, []byte("reviewed context"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instructionsPath, []byte("edited instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	cmd := newWorkspaceApplyInstructionPairTestCmd()
	setInstructionPairTestFlags(t, cmd, contextPath, instructionsPath, "reviewed context", "reviewed instructions")

	err := runWorkspaceApplyInstructionPair(cmd, []string{"11111111-1111-1111-1111-111111111111"})
	if err == nil || !strings.Contains(err.Error(), "candidate digest does not match") {
		t.Fatalf("err = %v, want reviewed candidate digest refusal", err)
	}
	if calls != 0 {
		t.Fatalf("HTTP calls = %d, want zero before refusing the changed candidate", calls)
	}
}
