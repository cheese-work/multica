package daemon

import (
	"fmt"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// contextCompactedTaskMessage turns a backend compaction event into the
// "context_compacted" run message. Input keeps the numbers machine-readable
// (trigger, tokens_before, tokens_after); Content is the human line.
func contextCompactedTaskMessage(msg agent.Message, seq int, at time.Time) TaskMessageData {
	content := "Context compacted"
	before, _ := msg.Input["tokens_before"].(int64)
	after, _ := msg.Input["tokens_after"].(int64)
	switch {
	case before > 0 && after > 0:
		content = fmt.Sprintf("Context compacted: %d -> %d tokens", before, after)
	case before > 0:
		content = fmt.Sprintf("Context compacted: %d tokens before", before)
	}
	return TaskMessageData{
		Seq:       seq,
		Type:      string(agent.MessageContextCompacted),
		Content:   content,
		Input:     msg.Input,
		CreatedAt: at,
	}
}
