package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// CHE-1300: CLI side of the owner-only guarded CAS — agent flags, stdin/file
// input, and ambiguous-failure reporting for all three governed fields.

func newAgentCASTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "update"}
	cmd.Flags().String("server-url", "", "")
	cmd.Flags().String("workspace-id", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("name", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().String("instructions", "", "")
	cmd.Flags().Bool("instructions-stdin", false, "")
	cmd.Flags().String("instructions-file", "", "")
	cmd.Flags().String("expected-before-digest", "", "")
	cmd.Flags().String("output", "json", "")
	return cmd
}

func writeCASFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "candidate.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write candidate file: %v", err)
	}
	return path
}

func TestBuildAgentUpdateDigestBody(t *testing.T) {
	t.Run("file input is read byte for byte", func(t *testing.T) {
		want := "line one\r\n\nüñí ☕\n\n"
		cmd := newAgentCASTestCmd()
		_ = cmd.Flags().Set("instructions-file", writeCASFile(t, want))
		_ = cmd.Flags().Set("expected-before-digest", testDigestHex)

		body, err := buildAgentUpdateDigestBody(cmd)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(body) != 2 || body["instructions"] != want || body["expected_before_digest"] != testDigestHex {
			t.Fatalf("body = %v, want exactly instructions %q + expected_before_digest", body, want)
		}
	})

	t.Run("stdin input is read byte for byte", func(t *testing.T) {
		want := "from stdin\n\n"
		cmd := newAgentCASTestCmd()
		cmd.SetIn(strings.NewReader(want))
		_ = cmd.Flags().Set("instructions-stdin", "true")
		_ = cmd.Flags().Set("expected-before-digest", testDigestHex)

		body, err := buildAgentUpdateDigestBody(cmd)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if body["instructions"] != want {
			t.Errorf("instructions = %q, want %q", body["instructions"], want)
		}
	})

	t.Run("inline text is refused so candidate text stays out of arguments", func(t *testing.T) {
		cmd := newAgentCASTestCmd()
		_ = cmd.Flags().Set("instructions", "secret candidate")
		_ = cmd.Flags().Set("expected-before-digest", testDigestHex)

		_, err := buildAgentUpdateDigestBody(cmd)
		if err == nil || !strings.Contains(err.Error(), "--instructions-stdin") {
			t.Fatalf("err = %v, want inline refusal naming stdin/file", err)
		}
		if strings.Contains(err.Error(), "secret candidate") {
			t.Fatalf("error leaks candidate text: %v", err)
		}
	})

	t.Run("invalid UTF-8 rejected", func(t *testing.T) {
		cmd := newAgentCASTestCmd()
		_ = cmd.Flags().Set("instructions-file", writeCASFile(t, string([]byte{0xff, 0xfe})))
		_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
		if _, err := buildAgentUpdateDigestBody(cmd); err == nil || !strings.Contains(err.Error(), "valid UTF-8") {
			t.Fatalf("err = %v, want invalid UTF-8 rejection", err)
		}
	})

	t.Run("malformed digest rejected", func(t *testing.T) {
		cmd := newAgentCASTestCmd()
		_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "x"))
		_ = cmd.Flags().Set("expected-before-digest", "short")
		if _, err := buildAgentUpdateDigestBody(cmd); err == nil || !strings.Contains(err.Error(), "64 hex characters") {
			t.Fatalf("err = %v, want malformed digest rejection", err)
		}
	})

	t.Run("missing instructions input rejected", func(t *testing.T) {
		cmd := newAgentCASTestCmd()
		_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
		if _, err := buildAgentUpdateDigestBody(cmd); err == nil || !strings.Contains(err.Error(), "requires the new instructions") {
			t.Fatalf("err = %v, want missing-instructions rejection", err)
		}
	})

	t.Run("any other update flag conflicts", func(t *testing.T) {
		for _, flag := range []string{"name", "description"} {
			cmd := newAgentCASTestCmd()
			_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "x"))
			_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
			_ = cmd.Flags().Set(flag, "v")
			if _, err := buildAgentUpdateDigestBody(cmd); err == nil || !strings.Contains(err.Error(), "--"+flag) {
				t.Errorf("--%s: err = %v, want conflict rejection", flag, err)
			}
		}
	})
}

func TestRunAgentUpdateDigestModeSendsSingleFieldPUT(t *testing.T) {
	var gotBody map[string]any
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent":         map[string]any{"id": "agent-123"},
			"before_digest": "before123",
			"after_digest":  "after456",
		})
	}))
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newAgentCASTestCmd()
	_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "new text\n"))
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)

	out, err := captureStdout(t, func() error { return runAgentUpdate(cmd, []string{"agent-123"}) })
	if err != nil {
		t.Fatalf("runAgentUpdate: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if len(gotBody) != 2 || gotBody["instructions"] != "new text\n" || gotBody["expected_before_digest"] != testDigestHex {
		t.Errorf("body = %v, want exactly instructions + expected_before_digest", gotBody)
	}
	var printed map[string]any
	if err := json.Unmarshal([]byte(out), &printed); err != nil {
		t.Fatalf("decode stdout %q: %v", out, err)
	}
	if printed["before_digest"] != "before123" || printed["after_digest"] != "after456" {
		t.Errorf("printed = %v", printed)
	}
}

func TestRunAgentUpdateInstructionsFileWithoutDigestStillSendsPlainUpdate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "agent-123", "name": "a"})
	}))
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newAgentCASTestCmd()
	_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "plain\n"))
	if _, err := captureStdout(t, func() error { return runAgentUpdate(cmd, []string{"agent-123"}) }); err != nil {
		t.Fatalf("runAgentUpdate: %v", err)
	}
	if len(gotBody) != 1 || gotBody["instructions"] != "plain\n" {
		t.Errorf("body = %v, want only instructions", gotBody)
	}
}

// casCLICase drives one governed field's digest-mode command against a server.
type casCLICase struct {
	name string
	run  func(t *testing.T, candidate string) error
}

func casCLICases() []casCLICase {
	return []casCLICase{
		{"workspace.context", func(t *testing.T, candidate string) error {
			resetWorkspaceUpdateFlags(t)
			setStringFlag(t, "context-file", writeCASFile(t, candidate))
			setStringFlag(t, "expected-before-digest", testDigestHex)
			return runWorkspaceUpdate(workspaceUpdateCmd, []string{testWorkspaceUUID})
		}},
		{"squad.instructions", func(t *testing.T, candidate string) error {
			cmd := newSquadUpdateTestCmd()
			_ = cmd.Flags().Set("instructions-file", writeCASFile(t, candidate))
			_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
			return runSquadUpdate(cmd, []string{"squad-123"})
		}},
		{"agent.instructions", func(t *testing.T, candidate string) error {
			cmd := newAgentCASTestCmd()
			_ = cmd.Flags().Set("instructions-file", writeCASFile(t, candidate))
			_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
			return runAgentUpdate(cmd, []string{"agent-123"})
		}},
	}
}

func TestDigestModeAmbiguousFailuresAreReportedNeverRetried(t *testing.T) {
	const candidate = "CANDIDATE-TEXT-MUST-NOT-LEAK\n"
	sum := sha256.Sum256([]byte(candidate))
	wantAfter := fmt.Sprintf("%x", sum)

	failures := map[string]http.HandlerFunc{
		"500 response": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		},
		"dropped connection": func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		},
	}
	for _, c := range casCLICases() {
		for fname, handler := range failures {
			t.Run(c.name+"/"+fname, func(t *testing.T) {
				calls := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					handler(w, r)
				}))
				defer srv.Close()
				setSquadUpdateServerEnv(t, srv.URL)

				err := c.run(t, candidate)
				if err == nil {
					t.Fatal("expected error")
				}
				msg := err.Error()
				for _, want := range []string{"AMBIGUOUS", "do NOT retry", testDigestHex, wantAfter} {
					if !strings.Contains(msg, want) {
						t.Errorf("error missing %q: %v", want, msg)
					}
				}
				if strings.Contains(msg, "CANDIDATE-TEXT-MUST-NOT-LEAK") {
					t.Errorf("error leaks candidate text: %v", msg)
				}
				if calls != 1 {
					t.Errorf("server called %d times, want exactly 1 (never retried)", calls)
				}
			})
		}
	}
}

func TestDigestModeDefinitiveRefusalIsNotAmbiguous(t *testing.T) {
	for _, c := range casCLICases() {
		for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict} {
			t.Run(fmt.Sprintf("%s/%d", c.name, status), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":"refused"}`))
				}))
				defer srv.Close()
				setSquadUpdateServerEnv(t, srv.URL)

				err := c.run(t, "x")
				if err == nil {
					t.Fatal("expected error")
				}
				if strings.Contains(err.Error(), "AMBIGUOUS") {
					t.Errorf("%d is a definitive refusal, got ambiguous report: %v", status, err)
				}
			})
		}
	}
}
