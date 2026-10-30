package handler

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// rerunTaskSource seeds a completed run on an issue assigned to its agent, the
// shape upstream's run-targeted rerun (`rerun-task`) repeats.
func rerunTaskSource(t *testing.T, title string) (issueID, sourceID string) {
	t.Helper()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, context.Background())
	issueID = dbfx.Issue(t, title, testutil.Cols{"assignee_type": "agent", "assignee_id": agentID})
	sourceID = dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "completed"})
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID) })
	return issueID, sourceID
}

func rerunTaskRequest(t *testing.T, sourceID string, body map[string]any) *http.Request {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/tasks/"+sourceID+"/rerun", body), "taskId", sourceID)
	return withChatTestWorkspaceCtx(t, req)
}

// TestRerunTaskByUserRefusesInactiveProject keeps the fork's project admission
// gate (RequireActiveIssueProject + writeProjectAdmissionError) on upstream's
// run-targeted rerun entry: a run in a paused project is refused with the
// structured project_inactive reason and no run is created.
func TestRerunTaskByUserRefusesInactiveProject(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID, sourceID := rerunTaskSource(t, "rerun-task in paused project")
	dbfx.Exec(t, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, inactiveProject(t, "paused"))

	response := testutil.Call(t, testHandler.RerunTaskByUser, rerunTaskRequest(t, sourceID, map[string]any{"fresh": true}))
	response.Want(http.StatusForbidden)
	if response.Map()["reason_code"] != "project_inactive" {
		t.Errorf("want project_inactive refusal, got %s", response.Text())
	}
	if n := tasksOn(t, issueID); n != 1 {
		t.Errorf("tasks on issue = %d, want only the source run", n)
	}
}

// TestRerunReasonReachesAuditLog keeps CHE-525's audited force: the caller's
// reason lands on the "issue rerun accepted" log line for both the legacy
// issue rerun and the run-targeted rerun.
func TestRerunReasonReachesAuditLog(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	issueID, sourceID := rerunTaskSource(t, "audited rerun")
	const taskReason = "authorized force: rerun-task audit"
	testutil.Call(t, testHandler.RerunTaskByUser, rerunTaskRequest(t, sourceID, map[string]any{"reason": taskReason})).Want(http.StatusAccepted)

	const issueReason = "authorized force: issue rerun audit"
	dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE issue_id = $1 AND id <> $2`, issueID, sourceID)
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/rerun", map[string]any{"reason": issueReason}), "id", issueID)
	testutil.Call(t, testHandler.RerunIssue, req).Want(http.StatusAccepted)

	for _, reason := range []string{taskReason, issueReason} {
		found := false
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, `msg="issue rerun accepted"`) && strings.Contains(line, reason) {
				found = true
			}
		}
		if !found {
			t.Errorf("no \"issue rerun accepted\" log line carries %q\n---\n%s", reason, logs.String())
		}
	}
}
