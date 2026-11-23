package daemon

import (
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestContextCompactedTaskMessage(t *testing.T) {
	t.Parallel()
	at := time.Unix(1700000000, 0).UTC()
	got := contextCompactedTaskMessage(agent.NewContextCompactedMessage("auto", 180_000, 40_000), 7, at)
	if got.Type != "context_compacted" || got.Seq != 7 || !got.CreatedAt.Equal(at) {
		t.Fatalf("envelope = %+v", got)
	}
	if got.Content != "Context compacted: 180000 -> 40000 tokens" {
		t.Fatalf("content = %q", got.Content)
	}
	if got.Input["trigger"] != "auto" || got.Input["tokens_before"] != int64(180_000) || got.Input["tokens_after"] != int64(40_000) {
		t.Fatalf("input = %#v", got.Input)
	}
	got = contextCompactedTaskMessage(agent.NewContextCompactedMessage("auto", 0, 0), 8, at)
	if got.Content != "Context compacted" {
		t.Fatalf("unknown-size content = %q", got.Content)
	}
}
