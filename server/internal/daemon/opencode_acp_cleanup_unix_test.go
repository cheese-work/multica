//go:build linux || darwin

package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// fakeOpencodeACPCancel speaks the part of `opencode acp` a cancelled turn
// needs: the prompt starts a tool and stays open until session/cancel, which
// OpenCode answers with stopReason "cancelled".
const fakeOpencodeACPCancel = `#!/bin/sh
if [ "$1" = "export" ] || [ "$1" = "session" ]; then
  printf '%s' '{"info":{"id":"ses_fake"},"messages":[]}'
  exit 0
fi
prompt_id=""
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/^{"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}\n' "$id" ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_fake"}}\n' "$id" ;;
    *'"method":"session/prompt"'*)
      prompt_id=$id
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake","update":{"sessionUpdate":"tool_call","toolCallId":"call-1","name":"bash","title":"bash","kind":"execute","status":"pending","rawInput":{"cwd":"/w"}}}}'
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"in_progress","kind":"execute","title":"sleep 30","rawInput":{"command":"sleep 30","cwd":"/w"}}}}'
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"in_progress","kind":"execute","title":"sleep 30","rawInput":{"command":"sleep 30","cwd":"/w"}}}}' ;;
    *'"method":"session/cancel"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"cancelled"}}\n' "$prompt_id" ;;
  esac
done
`

type cancelAtFirstTool struct {
	agent.Backend
	cancel context.CancelFunc
}

func (b cancelAtFirstTool) Execute(ctx context.Context, prompt string, opts agent.ExecOptions) (*agent.Session, error) {
	s, err := b.Backend.Execute(ctx, prompt, opts)
	if err != nil {
		return s, err
	}
	input, output := s.Messages, make(chan agent.Message, 16)
	s.Messages = output
	go func() {
		defer close(output)
		var once sync.Once
		for msg := range input {
			output <- msg
			if msg.Type == agent.MessageToolUse {
				once.Do(b.cancel)
			}
		}
	}()
	return s, nil
}

// A cancelled OpenCode ACP turn hands its Result to the daemon after a clean
// teardown. The daemon must take that hand-off as the cleanup acknowledgement
// instead of declaring cleanup unconfirmed before the Result arrives.
func TestOpencodeACPCancelledTurnConfirmsCleanupThroughDaemon(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte(fakeOpencodeACPCancel), 0o755); err != nil {
		t.Fatal(err)
	}
	backend, err := agent.New("opencode", agent.Config{
		ExecutablePath: bin,
		CLIVersion:     "opencode v2.2.0",
		BuiltinRuntime: true,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	d := newTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, _, err := d.executeAndDrain(ctx, cancelAtFirstTool{Backend: backend, cancel: cancel}, "do it",
		agent.ExecOptions{Cwd: t.TempDir(), Timeout: 15 * time.Second}, slog.Default(), "opencode-acp-cancel", "", new(atomic.Int32))
	if err != nil {
		t.Fatalf("executeAndDrain: %v", err)
	}
	if result.Status != "cancelled" {
		t.Fatalf("cancelled OpenCode ACP turn = %+v; want cancelled after the backend's cleanup hand-off", result)
	}
	var tasks sync.WaitGroup
	d.waitForTaskCleanup(&tasks, time.Second)
	if !d.ShutdownCleanupConfirmed() {
		t.Fatal("shutdown marked cleanup unconfirmed although the OpenCode process group was torn down")
	}
}
