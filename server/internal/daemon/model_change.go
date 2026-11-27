package daemon

import (
	"fmt"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// modelChangeTaskMessage turns a backend model transition into the
// "model_change" run message. Input keeps the evidence machine-readable (see
// agent.MessageModelChange); Content is a short log line. The DM wording is
// the server's.
func modelChangeTaskMessage(msg agent.Message, seq int, at time.Time) TaskMessageData {
	phase, _ := msg.Input["phase"].(string)
	source, _ := msg.Input["source_model"].(string)
	target, _ := msg.Input["target_model"].(string)
	content := fmt.Sprintf("Model change (%s): %s", phase, source)
	if target != "" {
		content += " -> " + target
	}
	return TaskMessageData{
		Seq:       seq,
		Type:      string(agent.MessageModelChange),
		Content:   content,
		Input:     msg.Input,
		CreatedAt: at,
	}
}
