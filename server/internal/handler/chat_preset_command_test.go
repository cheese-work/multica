package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// sendPresetCommand posts a DM message as the session's creator.
func sendPresetCommand(t *testing.T, sessionID, content string) *testutil.Response {
	t.Helper()
	req := withChatTestWorkspaceCtx(t, newRequest("POST", "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": content}))
	return testutil.Call(t, testHandler.SendChatMessage, withURLParam(req, "sessionId", sessionID))
}

func dmPreset(t *testing.T, sessionID string) string {
	t.Helper()
	var name *string
	dbfx.QueryRow(t, `SELECT preset_name FROM chat_session WHERE id = $1`, sessionID).Scan(&name)
	return str(name)
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, query, args...).Scan(&n)
	return n
}

// lastNotice is the newest server-authored reply in the DM.
func lastNotice(t *testing.T, sessionID string) string {
	t.Helper()
	var content string
	dbfx.QueryRow(t, `SELECT content FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'compact_notice' ORDER BY created_at DESC, id DESC LIMIT 1`, sessionID).Scan(&content)
	return content
}

// /preset sets, shows and clears the DM preset without enqueuing a run or
// putting the command into a prompt.
func TestDMPresetCommand_SetsShowsAndClears(t *testing.T) {
	requireDB(t)
	agentID, sessionID, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	addPreset(t, agentID, "fast", presetSpec{thinking: sp("low")})
	addPreset(t, agentID, "big", presetSpec{ctxArgs: []string{"--autocompact", "200k"}})

	send := func(content string) *testutil.Response {
		return sendPresetCommand(t, sessionID, content).Want(http.StatusCreated)
	}

	r := send("/preset fast")
	if got := dmPreset(t, sessionID); got != "fast" {
		t.Fatalf("after /preset fast: preset_name = %s", got)
	}
	var resp struct {
		MessageID string `json:"message_id"`
		TaskID    string `json:"task_id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TaskID != "" || resp.MessageID == "" {
		t.Fatalf("a preset command starts no run and answers with a message: %s", r.Body.String())
	}
	if n := countRows(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sessionID); n != 0 {
		t.Fatalf("/preset enqueued %d tasks", n)
	}
	if n := countRows(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND role = 'user'`, sessionID); n != 0 {
		t.Fatalf("/preset left %d user messages that would reach the next prompt", n)
	}

	send("/preset")
	if note := lastNotice(t, sessionID); !strings.Contains(note, "fast") || !strings.Contains(note, "big") || !strings.Contains(note, "base") {
		t.Fatalf("/preset listing = %q, want fast, big and base", note)
	}

	send("/preset nope")
	if got := dmPreset(t, sessionID); got != "fast" {
		t.Fatalf("unknown name changed the preset to %s", got)
	}
	if note := lastNotice(t, sessionID); !strings.Contains(note, "nope") {
		t.Fatalf("unknown-name reply = %q", note)
	}

	send("/preset base")
	if got := dmPreset(t, sessionID); got != "base" {
		t.Fatalf("after /preset base: preset_name = %s", got)
	}
	send("/preset reset")
	if got := dmPreset(t, sessionID); got != "<NULL>" {
		t.Fatalf("after /preset reset: preset_name = %s", got)
	}
}

// S2: `/preset base` wins over a non-base agent default.
func TestDMPresetCommand_S2_BaseBeatsAgentDefault(t *testing.T) {
	requireDB(t)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
	addPreset(t, agentID, "dflt", presetSpec{thinking: sp("low"), isDefault: true})
	sendPresetCommand(t, sessionID, "/preset base").Want(http.StatusCreated)

	s, _ := claimAfter(t, agentID, sessionID, runtimeID, daemonID)

	if str(s.PresetName) != "base" || str(s.PresetSource) != "dm" || str(s.Thinking) != "high" {
		t.Fatalf("snapshot = %+v, want the agent row via dm", s)
	}
}

// S3: `/preset reset` hands the choice back to the agent default.
func TestDMPresetCommand_S3_ResetFallsBackToAgentDefault(t *testing.T) {
	requireDB(t)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
	addPreset(t, agentID, "dflt", presetSpec{thinking: sp("low"), isDefault: true})
	addPreset(t, agentID, "fast", presetSpec{thinking: sp("max")})
	sendPresetCommand(t, sessionID, "/preset fast").Want(http.StatusCreated)
	sendPresetCommand(t, sessionID, "/preset reset").Want(http.StatusCreated)

	s, _ := claimAfter(t, agentID, sessionID, runtimeID, daemonID)

	if str(s.PresetName) != "dflt" || str(s.PresetSource) != "agent_default" {
		t.Fatalf("snapshot = %+v, want dflt via agent_default", s)
	}
}

// I7: a user who cannot invoke the agent cannot set its DM preset.
func TestDMPresetCommand_I7_NeedsInvokeAccess(t *testing.T) {
	requireDB(t)
	agentID, sessionID, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	addPreset(t, agentID, "fast", presetSpec{thinking: sp("low")})
	ownerID := seedSecurityTestOwner(t, "preset-cmd-owner")
	dbfx.Exec(t, `UPDATE agent SET permission_mode = 'private', owner_id = $2 WHERE id = $1`, agentID, ownerID)

	// The runtime-access check refuses first (409) when the new owner's agent
	// is private; the invoke gate itself answers 403. Either way: refused.
	if r := sendPresetCommand(t, sessionID, "/preset fast"); r.Code != http.StatusForbidden && r.Code != http.StatusConflict {
		t.Fatalf("a caller who cannot use the agent got %d: %s", r.Code, r.Body.String())
	}

	if got := dmPreset(t, sessionID); got != "<NULL>" {
		t.Fatalf("a denied /preset changed preset_name to %s", got)
	}
	if n := countRows(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID); n != 0 {
		t.Fatalf("a denied /preset wrote %d messages", n)
	}
}

// A message that merely starts with the word is an ordinary message.
func TestDMPresetCommand_OnlyTheExactCommand(t *testing.T) {
	requireDB(t)
	agentID, sessionID, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	addPreset(t, agentID, "fast", presetSpec{})

	sendPresetCommand(t, sessionID, "/presets are great").Want(http.StatusCreated)

	if n := countRows(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sessionID); n != 1 {
		t.Fatalf("an ordinary message should enqueue one task, got %d", n)
	}
}

// A workflow step preset applies to the step's handler agent at claim.
func TestClaimPreset_WorkflowStepPreset(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Preset step runtime")
	daemonID := "preset-step-daemon"
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = 'claude', daemon_id = $2 WHERE id = $1`, runtimeID, daemonID)
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Preset step agent")
	dbfx.Exec(t, `UPDATE agent SET model = 'claude-opus-5-5' WHERE id = $1`, agentID)
	addPreset(t, agentID, "dflt", presetSpec{thinking: sp("low"), isDefault: true})
	addPreset(t, agentID, "deep", presetSpec{thinking: sp("max")})

	steps, _ := json.Marshal([]map[string]any{{
		"status_key": "in_progress", "handler": map[string]any{"type": "agent", "id": agentID, "preset": "deep"}, "instructions": "go",
	}})
	var workflowID string
	dbfx.QueryRow(t, `INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps) VALUES ($1, 'preset step wf', 'in_progress', $2) RETURNING id`,
		testWorkspaceID, steps).Scan(&workflowID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE issue SET workflow_id = NULL WHERE id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM issue_workflow WHERE id = $1`, workflowID)
	})
	dbfx.Exec(t, `UPDATE issue SET workflow_id = $2 WHERE id = $1`, issueID, workflowID)
	taskID := seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET workflow_step = 'in_progress' WHERE id = $1`, taskID)

	claimForkTask(t, runtimeID, daemonID)

	s := readSnapshot(t, taskID)
	if str(s.PresetName) != "deep" || str(s.PresetSource) != "workflow_step" || str(s.Thinking) != "max" {
		t.Fatalf("snapshot = %+v, want deep via workflow_step", s)
	}
}
