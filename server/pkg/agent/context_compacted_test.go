package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewContextCompactedMessageShape(t *testing.T) {
	t.Parallel()
	got := NewContextCompactedMessage("auto", 180_000, 40_000)
	if got.Type != MessageContextCompacted || string(got.Type) != "context_compacted" {
		t.Fatalf("type = %q, want context_compacted", got.Type)
	}
	want := map[string]any{"trigger": "auto", "tokens_before": int64(180_000), "tokens_after": int64(40_000)}
	if !reflect.DeepEqual(got.Input, want) {
		t.Fatalf("input = %#v, want %#v", got.Input, want)
	}
	// Unknown counts are omitted, never written as a misleading zero.
	got = NewContextCompactedMessage("", 0, 0)
	if len(got.Input) != 0 {
		t.Fatalf("unknown counts must be omitted, got %#v", got.Input)
	}
}

func collectClaudeMessages(t *testing.T, frames string) []Message {
	t.Helper()
	fixturePath := filepath.Join(t.TempDir(), "frames.jsonl")
	if err := os.WriteFile(fixturePath, []byte(frames), 0o600); err != nil {
		t.Fatal(err)
	}
	fakePath := filepath.Join(t.TempDir(), "claude")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\nIFS= read -r _\ncat "+fixturePath+"\n"))
	backend, err := New("claude", Config{ExecutablePath: fakePath, Env: map[string]string{"IS_SANDBOX": "1"}, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var msgs []Message
	for m := range session.Messages {
		msgs = append(msgs, m)
	}
	<-session.Result
	return msgs
}

// Frame shape taken from Claude Code 2.1.318's SDK schema: system/compact_boundary
// with compact_metadata {trigger, pre_tokens, post_tokens?}.
func TestClaudeEmitsContextCompactedOnCompactBoundary(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	frames := `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"system","subtype":"compact_boundary","session_id":"s1","compact_metadata":{"trigger":"auto","pre_tokens":180000,"post_tokens":40000}}
{"type":"system","subtype":"compact_boundary","session_id":"s1","compact_metadata":{"trigger":"manual","pre_tokens":90000}}
{"type":"result","subtype":"success","session_id":"s1","result":"ok"}
`
	var got []Message
	for _, m := range collectClaudeMessages(t, frames) {
		if m.Type == MessageContextCompacted {
			got = append(got, m)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d context_compacted messages, want 2: %#v", len(got), got)
	}
	if want := NewContextCompactedMessage("auto", 180_000, 40_000); !reflect.DeepEqual(got[0], want) {
		t.Fatalf("first = %#v, want %#v", got[0], want)
	}
	if want := NewContextCompactedMessage("manual", 90_000, 0); !reflect.DeepEqual(got[1], want) {
		t.Fatalf("second = %#v, want %#v", got[1], want)
	}
}

func TestClaudeSystemFramesWithoutCompactionEmitNoContextCompacted(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	frames := `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"result","subtype":"success","session_id":"s1","result":"ok"}
`
	for _, m := range collectClaudeMessages(t, frames) {
		if m.Type == MessageContextCompacted {
			t.Fatalf("unexpected context_compacted: %#v", m)
		}
	}
}

func TestCodexEmitsContextCompactedOnContextCompactionItem(t *testing.T) {
	t.Parallel()
	var msgs []Message
	c, _, _ := newTestCodexClient(t)
	c.onMessage = func(m Message) { msgs = append(msgs, m) }
	c.threadID = "thread-1"
	c.setActiveTurnID("turn-1")
	// Prompt size of the last model call before compaction.
	c.handleRawNotification("thread/tokenUsage/updated", codexThreadTokenUsageParams(
		"thread-1", "turn-1",
		map[string]any{"inputTokens": float64(250_000), "outputTokens": float64(10)},
		map[string]any{"inputTokens": float64(250_000), "outputTokens": float64(10)},
	))
	item := func(typ string) map[string]any {
		return map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"type": typ, "id": "item-9"}}
	}
	c.handleRawNotification("item/started", item("contextCompaction"))
	c.handleRawNotification("item/completed", item("contextCompaction"))
	c.handleRawNotification("item/completed", item("reasoning"))

	var got []Message
	for _, m := range msgs {
		if m.Type == MessageContextCompacted {
			got = append(got, m)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d context_compacted messages, want exactly 1 (completed only): %#v", len(got), got)
	}
	// Codex reports no post-compaction size on the item; it is omitted, not zero.
	if want := NewContextCompactedMessage("auto", 250_000, 0); !reflect.DeepEqual(got[0], want) {
		t.Fatalf("got %#v, want %#v", got[0], want)
	}
}

func TestSendReliableWaitsForRoomAndStopsOnCancel(t *testing.T) {
	t.Parallel()
	ch := make(chan Message, 1)
	ch <- Message{Type: MessageText}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- sendReliable(ctx, ch, NewContextCompactedMessage("auto", 1, 0)) }()
	select {
	case <-done:
		t.Fatal("sendReliable returned while the channel was full")
	case <-time.After(50 * time.Millisecond):
	}
	<-ch // make room: the pending send must now land
	if !<-done {
		t.Fatal("sendReliable should deliver once there is room")
	}
	if got := <-ch; got.Type != MessageContextCompacted {
		t.Fatalf("got %#v", got)
	}
	ch <- Message{Type: MessageText}
	cancel()
	if sendReliable(ctx, ch, NewContextCompactedMessage("auto", 1, 0)) {
		t.Fatal("must give up when ctx is done and the channel is full")
	}
}

// A saturated message channel must not drop the compaction record (CHE-9299 review P1).
// The consumer starts reading only after 300 log frames have overflowed the 256-slot channel.
func TestClaudeCompactionSurvivesSaturatedChannel(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	var sb strings.Builder
	sb.WriteString(`{"type":"system","subtype":"init","session_id":"s1"}` + "\n")
	for i := 0; i < 300; i++ {
		sb.WriteString(`{"type":"log","log":{"level":"info","message":"x"}}` + "\n")
	}
	sb.WriteString(`{"type":"system","subtype":"compact_boundary","session_id":"s1","compact_metadata":{"trigger":"auto","pre_tokens":9,"post_tokens":3}}` + "\n")
	sb.WriteString(`{"type":"result","subtype":"success","session_id":"s1","result":"ok"}` + "\n")

	fixturePath := filepath.Join(t.TempDir(), "frames.jsonl")
	if err := os.WriteFile(fixturePath, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	fakePath := filepath.Join(t.TempDir(), "claude")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\nIFS= read -r _\ncat "+fixturePath+"\n"))
	backend, err := New("claude", Config{ExecutablePath: fakePath, Env: map[string]string{"IS_SANDBOX": "1"}, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // let the channel fill before anyone reads
	found := 0
	for m := range session.Messages {
		if m.Type == MessageContextCompacted {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("context_compacted messages = %d, want 1 (dropped on a saturated channel)", found)
	}
}
