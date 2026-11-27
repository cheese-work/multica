package daemon

import (
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestModelChangeTaskMessage(t *testing.T) {
	t.Parallel()
	at := time.Unix(1700000000, 0).UTC()
	msg := agent.Message{Type: agent.MessageModelChange, Input: map[string]any{
		"phase": "attempt", "source_model": "claude-opus-5-5", "target_model": "claude-opus-4-8", "native_event_id": "u1",
	}}
	got := modelChangeTaskMessage(msg, 3, at)
	if got.Type != "model_change" || got.Seq != 3 || !got.CreatedAt.Equal(at) {
		t.Fatalf("envelope = %+v", got)
	}
	if got.Content != "Model change (attempt): claude-opus-5-5 -> claude-opus-4-8" {
		t.Fatalf("content = %q", got.Content)
	}
	if got.Input["native_event_id"] != "u1" {
		t.Fatalf("input = %#v", got.Input)
	}
	failed := modelChangeTaskMessage(agent.Message{Input: map[string]any{"phase": "failed", "source_model": "m"}}, 4, at)
	if failed.Content != "Model change (failed): m" {
		t.Fatalf("no-target content = %q", failed.Content)
	}
}
