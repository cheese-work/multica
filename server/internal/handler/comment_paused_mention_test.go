package handler

import (
	"fmt"
	"testing"
)

// CHE-5066: an @mention of a scheduling-paused agent starts no run and is
// reported as blocked with agent_paused; resuming lifts the block.
func TestCommentMention_PausedAgentBlocked(t *testing.T) {
	assigneeID := createHandlerTestAgent(t, "Paused Mention Assignee", nil)
	targetID := createHandlerTestAgent(t, "Paused Mention Target", nil)
	issueID := createCommentTriggerPreviewIssue(t, "paused agent mention", "agent", assigneeID)
	content := fmt.Sprintf("[@Target](mention://agent/%s) please look", targetID)

	dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = now() WHERE id = $1`, targetID)
	preview := previewCommentTriggersForTest(t, issueID, map[string]any{"content": content})
	if len(preview.Agents) != 0 || len(preview.Blocked) != 1 || preview.Blocked[0].ReasonCode != ReasonAgentPaused {
		t.Fatalf("paused mention preview = agents %+v blocked %+v, want 0 agents and one agent_paused block", preview.Agents, preview.Blocked)
	}

	dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = NULL WHERE id = $1`, targetID)
	preview = previewCommentTriggersForTest(t, issueID, map[string]any{"content": content})
	if len(preview.Blocked) != 0 || len(preview.Agents) != 1 {
		t.Fatalf("resumed mention preview = agents %+v blocked %+v, want 1 agent and no block", preview.Agents, preview.Blocked)
	}
}
