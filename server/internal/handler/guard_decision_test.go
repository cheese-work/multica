package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func guardUpload(t *testing.T, taskID string, decisions []map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParam(newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/guard-decisions",
		map[string]any{"decisions": decisions}), "taskId", taskID)
	return testutil.Call(t, testHandler.ReportGuardDecisions, req)
}

func TestGuardDecisionsUploadListAndStats(t *testing.T) {
	issueID := dbfx.Issue(t, "guard decisions")
	agentID := dbfx.Agent(t, "guard decisions agent", testRuntimeID)
	taskID := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": testRuntimeID, "status": "running"})
	now := time.Now().UTC()
	rows := []map[string]any{
		{"seq": 0, "time": now.Format(time.RFC3339), "plugin": "pr-title-tag", "action": "fixed", "tool": "gh", "subcommand": "pr create", "line": "title tag [merge] added", "issue": "CHE-1"},
		{"seq": 1, "time": now.Format(time.RFC3339), "plugin": "pr-title-tag", "action": "refused", "tool": "gh", "subcommand": "pr create", "line": "pass --title explicitly"},
		{"seq": 2, "time": now.Format(time.RFC3339), "plugin": "ci-wait", "action": "plugin-error", "tool": "gh", "subcommand": "pr checks", "note": "boom"},
	}

	// A retried upload adds nothing; a bad action or an unrelated daemon writes nothing.
	guardUpload(t, taskID, rows).Want(http.StatusOK)
	guardUpload(t, taskID, rows).Want(http.StatusOK)
	guardUpload(t, taskID, []map[string]any{{"seq": 9, "time": now.Format(time.RFC3339), "plugin": "x", "action": "bogus"}}).Want(http.StatusBadRequest)
	denied := withURLParam(newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/guard-decisions",
		map[string]any{"decisions": rows}, "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002"), "taskId", taskID)
	testutil.Call(t, testHandler.ReportGuardDecisions, denied).Want(http.StatusNotFound)

	var list struct {
		Decisions []struct {
			Plugin, Action, Line, Issue, Runtime string
		}
	}
	testutil.Call(t, testHandler.ListGuardDecisions, newRequest(http.MethodGet, "/api/guard/decisions?plugin=pr-title-tag", nil)).
		Want(http.StatusOK).JSON(&list)
	if len(list.Decisions) != 2 {
		t.Fatalf("decisions = %+v, want the 2 pr-title-tag rows once each", list.Decisions)
	}
	if list.Decisions[0].Runtime == "" {
		t.Fatalf("runtime not stamped from the task: %+v", list.Decisions[0])
	}

	var stats struct {
		Days    int
		Plugins []struct {
			Plugin                 string
			Fixed, Refused, Errors int64
			Daily                  []struct {
				Day                    string
				Fixed, Refused, Errors int64
			}
		}
	}
	testutil.Call(t, testHandler.GetGuardStats, newRequest(http.MethodGet, "/api/guard/stats?days=7&tz=Asia/Ho_Chi_Minh", nil)).
		Want(http.StatusOK).JSON(&stats)
	got := map[string][3]int64{}
	for _, p := range stats.Plugins {
		got[p.Plugin] = [3]int64{p.Fixed, p.Refused, p.Errors}
		if len(p.Daily) != 7 {
			t.Fatalf("%s daily has %d days, want 7 dense days", p.Plugin, len(p.Daily))
		}
	}
	if got["pr-title-tag"] != [3]int64{1, 1, 0} || got["ci-wait"] != [3]int64{0, 0, 1} {
		t.Fatalf("stats = %+v", got)
	}
}
