package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Compact change cancels live compaction tasks (CHE-2908, SEC-1 rule 4).

// compactTask queues a task for agentID on a fresh DM of wsID. kind "" is an
// ordinary chat task; "compact" is a compaction run.
func compactTask(t *testing.T, wsID, agentID, status, kind string) string {
	t.Helper()
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id=$1`, agentID).Scan(&runtimeID)
	cols := testutil.Cols{
		"runtime_id":      runtimeID,
		"status":          status,
		"chat_session_id": dbfx.ChatSession(t, agentID, testutil.Cols{"workspace_id": wsID}),
	}
	if kind != "" {
		cols["task_kind"] = kind
	}
	if status == "running" {
		cols["started_at"] = testutil.Raw("now()")
	}
	return dbfx.Task(t, agentID, cols)
}

func wantTasks(t *testing.T, want string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if got := taskStatus(t, id); got != want {
			t.Fatalf("task %s status = %s, want %s", id, got, want)
		}
	}
}

func TestBackgroundAgentsCompactChangeCancelsLiveCompactionTasks(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bg-cancel")
	putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)

	live := []string{
		compactTask(t, wsID, a, "queued", "compact"),
		compactTask(t, wsID, a, "deferred", "compact"),
		compactTask(t, wsID, a, "dispatched", "compact"),
		compactTask(t, wsID, a, "running", "compact"),
		compactTask(t, wsID, a, "waiting_local_directory", "compact"),
		compactTask(t, wsID, b, "queued", "compact"), // queued for another agent
	}
	ordinary := compactTask(t, wsID, a, "queued", "")
	done := compactTask(t, wsID, a, "completed", "compact")
	otherWS, otherA, _, _ := bgFixture(t, "bg-cancel-other")
	foreign := compactTask(t, otherWS, otherA, "queued", "compact")

	putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": b}}, nil).Want(http.StatusOK)

	wantTasks(t, "cancelled", live...)
	wantTasks(t, "queued", ordinary, foreign)
	wantTasks(t, "completed", done)
}

func TestBackgroundAgentsOtherJobChangeKeepsCompactionTasks(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bg-keep")
	queued := compactTask(t, wsID, a, "queued", "compact")
	putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": b}}, nil).Want(http.StatusOK)
	wantTasks(t, "queued", queued)
}

func TestBackgroundAgentsUnchangedCompactKeepsCompactionTasks(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bg-same")
	putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	queued := compactTask(t, wsID, a, "queued", "compact")
	putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	wantTasks(t, "queued", queued)
}

func TestBackgroundAgentsCompactResetCancelsLiveCompactionTasks(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bg-reset-cancel")
	putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	queued := compactTask(t, wsID, a, "queued", "compact")
	bgCall(t, testHandler.ResetBackgroundAgents, "DELETE", wsID, nil, nil).Want(http.StatusOK)
	wantTasks(t, "cancelled", queued)
}

func TestBackgroundAgentsRefusedMachineCompactWriteCancelsNothing(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bg-refused-cancel")
	putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	queued := compactTask(t, wsID, a, "queued", "compact")
	events := len(bgEventsFor(wsID))

	wantCode(t, putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": b}}, machine("task_token")),
		http.StatusForbidden, codeBackgroundAgentsHumanOnly)
	wantCode(t, settingsCall(t, "PATCH", wsID, map[string]any{"background_agents": map[string]any{"compact": map[string]any{"agent_id": b}}}, machine("task_token")),
		http.StatusForbidden, codeBackgroundAgentsHumanOnly)

	wantTasks(t, "queued", queued)
	if got := len(bgEventsFor(wsID)); got != events {
		t.Fatalf("events = %d, want %d: a refused write published", got, events)
	}
}

func TestBackgroundAgentsIneligibleCompactWriteCancelsNothing(t *testing.T) {
	wsID, a, _, private := bgFixture(t, "bg-ineligible-cancel")
	putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	queued := compactTask(t, wsID, a, "queued", "compact")
	wantCode(t, putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": private}}, nil),
		http.StatusBadRequest, codeAgentNotEligible)
	wantTasks(t, "queued", queued)
}
