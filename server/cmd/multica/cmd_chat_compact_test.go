package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

func chatCompactTestCommand(summaryFile string) *cobra.Command {
	command := &cobra.Command{Use: "compact"}
	command.Flags().String("summary-file", summaryFile, "")
	command.Flags().String("output", "json", "")
	return command
}

func writeSummary(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestChatCompactPostsSummaryToTheCompactEndpoint(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat/compact" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.Contains(body["summary"], "Decision: keep evidence.") {
			t.Errorf("summary request: %v %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"compaction_epoch":1}`))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	var out bytes.Buffer
	command := chatCompactTestCommand(writeSummary(t, "Goal: continue.\nDecision: keep evidence."))
	command.SetOut(&out)
	if err := runChatCompact(command, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"compaction_epoch"`) {
		t.Fatalf("missing response: %s", out.String())
	}
}

func TestChatCompactRejectsInvalidSummaryWithoutHTTP(t *testing.T) {
	for _, content := range []string{"", " \n", string([]byte{0xff}), strings.Repeat("x", 65537)} {
		if err := runChatCompact(chatCompactTestCommand(writeSummary(t, content)), nil); err == nil {
			t.Fatalf("invalid summary %q was accepted", content[:min(len(content), 8)])
		}
	}
	if err := runChatCompact(chatCompactTestCommand(""), nil); err == nil {
		t.Fatal("missing --summary-file was accepted")
	}
}

// V1: a server without in-place compaction answers 404 or 405. The command
// must stop with the DM unchanged and must never reach /api/chat/continue,
// which would archive the DM into a new one.
func TestChatCompactAgainstAnOldServerFailsClosed(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		clearAmbientDaemonTaskEnv(t)
		var mu sync.Mutex
		var paths []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			http.Error(w, "404 page not found", status)
		}))
		setCLITestServerEnv(t, server.URL)
		err := runChatCompact(chatCompactTestCommand(writeSummary(t, "Goal: continue.")), nil)
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "server does not support in-place compaction; DM unchanged") {
			t.Fatalf("status %d: err = %v, want the unsupported-server message", status, err)
		}
		mu.Lock()
		for _, p := range paths {
			if p != "/api/chat/compact" {
				t.Errorf("status %d: also called %s; a skewed server must never fall back to another path", status, p)
			}
		}
		mu.Unlock()
	}
}

// A 404 that names the missing chat session is the endpoint speaking, not an
// old server; it keeps its own message.
func TestChatCompactKeepsTheServersOwnNotFound(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"chat session not found"}`))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	err := runChatCompact(chatCompactTestCommand(writeSummary(t, "Goal: continue.")), nil)
	if err == nil || strings.Contains(err.Error(), "does not support") || !strings.Contains(err.Error(), "chat session not found") {
		t.Fatalf("err = %v, want the server's own not-found", err)
	}
}

func TestChatCompactCommandIsRegistered(t *testing.T) {
	cmd, _, err := chatCmd.Find([]string{"compact"})
	if err != nil || cmd.Name() != "compact" || cmd.Flags().Lookup("summary-file") == nil {
		t.Fatalf("multica chat compact --summary-file is not registered: %v", err)
	}
}
