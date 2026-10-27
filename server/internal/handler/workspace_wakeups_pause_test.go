package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Workspace wakeup pause (CHE-1727): the switch lives in workspace.settings,
// the server stamps who and when, and each flip leaves one activity record.

func pauseWorkspaceFixture(t *testing.T, slug string) string {
	t.Helper()
	wsID := dbfx.Insert(t, "workspace", testutil.Cols{"name": slug, "slug": slug, "description": "", "issue_prefix": "WPZ"})
	dbfx.Exec(t, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, wsID, testUserID)
	dbfx.Cleanup(t, "DELETE FROM activity_log WHERE workspace_id=$1", wsID)
	return wsID
}

func saveWorkspaceSettings(t *testing.T, wsID string, settings map[string]any) map[string]any {
	t.Helper()
	req := withURLParam(newRequest("PATCH", "/api/workspaces/"+wsID, map[string]any{"settings": settings}), "id", wsID)
	var out struct {
		Settings map[string]any `json:"settings"`
	}
	testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusOK).JSON(&out)
	return out.Settings
}

func pauseActivities(t *testing.T, wsID, action string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE workspace_id=$1 AND action=$2 AND actor_type='member' AND actor_id=$3`, wsID, action, testUserID).Scan(&n)
	return n
}

func TestUpdateWorkspaceStampsWakeupPauseAndAudits(t *testing.T) {
	wsID := pauseWorkspaceFixture(t, "wakeup-pause-audit")
	got := saveWorkspaceSettings(t, wsID, map[string]any{
		"wakeups_paused": true, "wakeups_paused_by": "forged-id", "wakeups_paused_at": "2001-01-01T00:00:00Z",
	})
	if got["wakeups_paused"] != true || got["wakeups_paused_by"] != testUserID {
		t.Fatalf("pause not stamped by the server: %v", got)
	}
	at, _ := got["wakeups_paused_at"].(string)
	if at == "" || at == "2001-01-01T00:00:00Z" {
		t.Fatalf("pause time must come from the server: %q", at)
	}
	if n := pauseActivities(t, wsID, "wakeups_paused"); n != 1 {
		t.Fatalf("pause activity records = %d, want 1", n)
	}
	// An unrelated save by a client that never saw the pause keeps it, and an
	// echo of the pause does not restamp it or add a record.
	kept := saveWorkspaceSettings(t, wsID, map[string]any{"github_pr_sidebar_enabled": false})
	if kept["wakeups_paused"] != true || kept["wakeups_paused_at"] != at || kept["wakeups_paused_by"] != testUserID {
		t.Fatalf("unrelated save changed the pause: %v", kept)
	}
	saveWorkspaceSettings(t, wsID, map[string]any{"wakeups_paused": true})
	if n := pauseActivities(t, wsID, "wakeups_paused"); n != 1 {
		t.Fatalf("an unchanged pause wrote %d records, want 1", n)
	}
	resumed := saveWorkspaceSettings(t, wsID, map[string]any{"wakeups_paused": false})
	if _, paused := resumed["wakeups_paused"]; paused || resumed["wakeups_paused_by"] != nil || resumed["wakeups_paused_at"] != nil {
		t.Fatalf("resume left pause keys behind: %v", resumed)
	}
	if n := pauseActivities(t, wsID, "wakeups_resumed"); n != 1 {
		t.Fatalf("resume activity records = %d, want 1", n)
	}
}

func TestIssueWakeupCreateWarnsWhilePaused(t *testing.T) {
	wsID := pauseWorkspaceFixture(t, "wakeup-pause-warn")
	runtime := dbfx.Runtime(t, "pause warn", testutil.Cols{"workspace_id": wsID})
	agent := dbfx.Agent(t, "pause warn", runtime, testutil.Cols{"workspace_id": wsID})
	issue := dbfx.Issue(t, "pause warn", testutil.Cols{"workspace_id": wsID})
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN(SELECT id FROM issue_wakeup WHERE issue_id=$1)", issue)
	dbfx.Cleanup(t, "DELETE FROM activity_log WHERE issue_id=$1", issue)
	create := func() []string {
		t.Helper()
		body := map[string]any{"agent_id": agent, "kind": "at", "after_seconds": 600, "instruction": "check"}
		req := withURLParam(newRequest("POST", "/api/issues/"+issue+"/wakeups", body), "id", issue)
		req.Header.Set("X-Workspace-ID", wsID)
		rec := httptest.NewRecorder()
		testHandler.CreateIssueWakeup(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Warnings []string `json:"warnings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Warnings
	}
	if w := create(); len(w) != 0 {
		t.Fatalf("unpaused create warned: %v", w)
	}
	dbfx.Exec(t, `UPDATE workspace SET settings=COALESCE(settings,'{}'::jsonb)||'{"wakeups_paused":true}'::jsonb WHERE id=$1`, wsID)
	if w := create(); len(w) != 1 || w[0] != "workspace_wakeups_paused" {
		t.Fatalf("paused create warnings = %v, want [workspace_wakeups_paused]", w)
	}
}
