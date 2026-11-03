package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// writeFakeCodex writes a POSIX app-server under the fork lock, so no child
// started meanwhile inherits the write descriptor (ETXTBSY on exec).
func writeFakeCodex(t *testing.T, path, script string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
}

// The provider accepts turn/steer and never answers it. The real Codex steer
// deadline expires while the provider may hold the text, so the daemon's
// timeout ack must leave the receipt uncertain, not failed.
func TestChatSupplementCodexSteerAcceptedButResponseTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	f := steeredFixture(t)
	dir := t.TempDir()
	received := filepath.Join(dir, "received.log")
	codexPath := filepath.Join(dir, "codex")
	writeFakeCodex(t, codexPath, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli 0.0.0-test"; exit 0; fi
read line
echo '{"jsonrpc":"2.0","id":1,"result":{}}'
read line
read line
echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-steer"}}}'
read line
echo '{"jsonrpc":"2.0","id":3,"result":{}}'
echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-steer","turn":{"id":"turn-steer"}}}'
while IFS= read -r line; do printf '%s\n' "$line" >> '`+received+`'; done
`)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	backend, err := agent.New("codex", agent.Config{ExecutablePath: codexPath, Logger: quiet})
	if err != nil {
		t.Fatalf("codex backend: %v", err)
	}
	runCtx, endRun := context.WithCancel(context.Background())
	session, err := backend.Execute(runCtx, "first turn", agent.ExecOptions{Timeout: time.Minute, EnableTaskSupplement: true})
	if err != nil {
		endRun()
		t.Fatalf("execute fake codex: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	t.Cleanup(func() {
		endRun()
		select {
		case <-session.Result:
		case <-time.After(30 * time.Second):
			t.Error("fake codex run did not stop")
		}
	})
	for deadline := time.Now().Add(10 * time.Second); !session.SupplementReady(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("fake codex turn never became steerable")
		}
	}

	type ack struct {
		Delivered bool   `json:"delivered"`
		Error     string `json:"error"`
	}
	var mu sync.Mutex
	var acks []ack
	client := newObservedChatSupplementDaemonClient(t, func(r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/ack") {
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var a ack
		if err := json.Unmarshal(body, &a); err != nil {
			t.Errorf("ack body %q: %v", body, err)
		}
		mu.Lock()
		acks = append(acks, a)
		mu.Unlock()
	})
	sup := claimSteer(t, client, f)
	daemon.DeliverTaskSupplement(runCtx, client, session, f.runningID, sup, quiet)

	raw, err := os.ReadFile(received)
	if err != nil {
		t.Fatalf("read provider input: %v", err)
	}
	var steers int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var req struct {
			Method string `json:"method"`
			Params struct {
				ExpectedTurnID string `json:"expectedTurnId"`
				Input          []struct {
					Text string `json:"text"`
				} `json:"input"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &req) != nil || req.Method != "turn/steer" {
			continue
		}
		steers++
		if req.Params.ExpectedTurnID != "turn-steer" || len(req.Params.Input) != 1 || !strings.Contains(req.Params.Input[0].Text, "steer me") {
			t.Fatalf("provider received steer %s", line)
		}
	}
	if steers != 1 {
		t.Fatalf("provider received %d turn/steer requests, want 1:\n%s", steers, raw)
	}
	mu.Lock()
	gotAcks := append([]ack(nil), acks...)
	mu.Unlock()
	if len(gotAcks) != 1 || gotAcks[0].Delivered || gotAcks[0].Error != "timeout" {
		t.Fatalf("daemon acks = %+v, want one delivered=false reason timeout", gotAcks)
	}

	if row := readChatReceipt(t, f); row.status != "uncertain" || row.reason != "timeout" || row.attempts != 1 {
		t.Fatalf("receipt = %+v, want uncertain / timeout", row)
	}
	wantConflictCode(t, resendChat(t, f, testUserID, false), "supplement_delivery_uncertain")

	req := withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+f.sessionID+"/messages", nil), "sessionId", f.sessionID)
	var messages []ChatMessageResponse
	testutil.Call(t, testHandler.ListChatMessages, chatPendingCtxAs(t, req, testUserID)).Want(http.StatusOK).JSON(&messages)
	for _, m := range messages {
		if m.ID != f.messageID {
			continue
		}
		if s := m.Supplement; s == nil || s.Status != "uncertain" || s.FailureReason == nil || *s.FailureReason != "timeout" {
			t.Fatalf("bubble receipt = %+v, want uncertain / timeout", s)
		}
		return
	}
	t.Fatalf("steered message %s missing from transcript", f.messageID)
}
