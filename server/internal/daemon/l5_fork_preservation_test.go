package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// CHE-3460 fork-preservation tests: each pins a behaviour the v0.8.0 sync kept
// at a conflict hunk, through the real runTask path rather than a helper.

// Hermes ACP fixture: one cumulative usage frame, then a held prompt so the
// live flush has time to tick before the terminal report.
const l5HermesUsageFixture = `#!/bin/sh
case "$*" in *--version*) echo '1.0.0'; exit 0;; esac
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{}}}\n' "$id";;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"test-session"}}\n' "$id";;
    *'"method":"session/prompt"'*)
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"test-session","update":{"sessionUpdate":"usage_update","size":1000,"used":10,"_meta":{"usage":{"inputTokens":111,"outputTokens":22}}}}}'
      sleep 1
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id";;
    *) if [ -n "$id" ]; then printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"; fi;;
  esac
done
`

// Grok ACP fixture that holds the prompt open, so the supplement loop runs.
const l5GrokSupplementFixture = `#!/bin/sh
case "$*" in *--version*) echo '1.0.20'; exit 0;; esac
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"authMethods":[{"id":"cached_token","name":"Test authentication"}],"agentCapabilities":{"mcpCapabilities":{"http":true,"sse":true}}}}\n' "$id";;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"test-session","models":{"currentModelId":"grok-4.6","availableModels":[{"modelId":"grok-4.6","name":"Test model"}]}}}\n' "$id";;
    *'"method":"session/prompt"'*)
      sleep 2
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id";;
    *) if [ -n "$id" ]; then printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"; fi;;
  esac
done
`

func l5Daemon(t *testing.T, provider, script string, handler http.HandlerFunc) (*Daemon, Task, *slog.Logger) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script agent fixture is POSIX-only")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := filepath.Join(t.TempDir(), provider+"-fixture")
	writeTestExecutable(t, fake, []byte(script))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	d := &Daemon{
		client: NewClient(srv.URL), logger: logger,
		workspaces: make(map[string]*workspaceState), activeEnvRoots: make(map[string]int),
		activeStores: make(map[string]int), deletingStores: make(map[string]bool),
		runtimeIndex: map[string]Runtime{"test-rt": {ID: "test-rt", Provider: provider}},
		cfg: Config{
			WorkspacesRoot: t.TempDir(), AgentTimeout: 10 * time.Second, ServerBaseURL: srv.URL,
			Agents: map[string]AgentEntry{provider: {Path: fake}},
		},
	}
	task := Task{
		ID: "test-task", WorkspaceID: "test-ws", RuntimeID: "test-rt", IssueID: "test-issue",
		AgentID: "test-agent", AuthToken: "mat_test_l5",
		Agent: &AgentData{ID: "test-agent", Name: "test", Model: "m"},
	}
	d.activeStoresCond = sync.NewCond(&d.activeStoresMu)
	return d, task, logger
}

// Fork behaviour: STOE-30 live usage flush stays wired into runTask. A naive
// "take upstream" hunk drops the OnUsage/startUsageFlush block, so no mid-run
// snapshot ever reaches the server.
func TestRunTaskFlushesLiveUsageSnapshotMidRun(t *testing.T) {
	old := liveUsageFlushEvery
	liveUsageFlushEvery = 100 * time.Millisecond
	t.Cleanup(func() { liveUsageFlushEvery = old })

	var mu sync.Mutex
	var snapshots []map[string]any
	d, task, logger := l5Daemon(t, "hermes", l5HermesUsageFixture, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if snap, _ := body["snapshot"].(bool); snap {
				mu.Lock()
				snapshots = append(snapshots, body)
				mu.Unlock()
			}
		}
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := d.runTask(context.Background(), task, "hermes", 0, logger); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(snapshots) == 0 {
		t.Fatal("no mid-run usage snapshot reached the server")
	}
}

// Fork behaviour: PR-event negotiation reaches the supplement loop. A naive
// hunk drops withTaskPREvents, so a run the server negotiated never claims
// PR events.
func TestRunTaskClaimsPREventsWhenNegotiated(t *testing.T) {
	var mu sync.Mutex
	prClaims := 0
	d, task, logger := l5Daemon(t, "grok", l5GrokSupplementFixture, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			_, _ = w.Write([]byte(`{"supplement_capability":"task-supplement-v1","pr_event_capability":"task-supplement-pr-event-v1"}`))
		case strings.HasSuffix(r.URL.Path, "/pr-events/claim"):
			mu.Lock()
			prClaims++
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/supplements/claim"):
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	if _, err := d.runTask(context.Background(), task, "grok", 0, logger); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if prClaims == 0 {
		t.Fatal("negotiated run never claimed PR events")
	}
}
