package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// failWorkspaceRead delegates to the real pool except for the GetWorkspace
// query, which always errors.
type failWorkspaceRead struct{ db.DBTX }

type workspaceErrRow struct{ err error }

func (r workspaceErrRow) Scan(...any) error { return r.err }

func (f failWorkspaceRead) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "name: GetWorkspace :one") {
		return workspaceErrRow{errors.New("simulated workspace read failure")}
	}
	return f.DBTX.QueryRow(ctx, sql, args...)
}

func (f failWorkspaceRead) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return f.DBTX.Exec(ctx, sql, args...)
}

// CHE-3706: when the agent has no explicit profile, the workspace default
// decides. A failed workspace read must not launch the task as personal.
func TestBuildClaimedTaskResponse_HarnessProfileWorkspaceReadFailureFailsClosed(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Harness profile read failure runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Harness profile read failure agent")
	seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)
	task, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, parseUUID(runtimeID))
	if err != nil || task == nil {
		t.Fatalf("claim task: %v %+v", err, task)
	}
	runtime, err := testHandler.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
		ID: parseUUID(runtimeID), WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}

	broken := *testHandler
	broken.Queries = db.New(failWorkspaceRead{testPool})
	req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil)
	resp, _, _, _, _, failure := broken.buildClaimedTaskResponse(req, task, runtime, runtimeID, testWorkspaceID)
	if failure == nil {
		profile := ""
		if resp.Agent != nil {
			profile = resp.Agent.HarnessProfile
		}
		t.Fatalf("claim succeeded with harness profile %q; want it to fail closed", profile)
	}
	if failure.status != http.StatusInternalServerError || failure.outcome != "error_harness_profile" {
		t.Fatalf("failure = %+v, want 500 error_harness_profile", failure)
	}
}

// CHE-3706: an older daemon drops harness_profile and launches the user's
// personal config. The server must never hand it a managed task; personal is
// unchanged for both daemon generations.
func TestBuildClaimedTaskResponse_ManagedProfileRequiresDaemonCapability(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name       string
		profile    string
		capability string
		wantFail   bool
	}{
		{"managed old daemon", "managed", "", true},
		{"managed other capabilities only", "managed", protocol.DaemonCapabilitySkillBundlesV1, true},
		{"managed new daemon", "managed", protocol.DaemonCapabilityRuntimeProfileV1, false},
		{"personal old daemon", "personal", "", false},
		{"personal new daemon", "personal", protocol.DaemonCapabilityRuntimeProfileV1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			runtimeID := createClaimReclaimRuntime(t, ctx, "Runtime profile capability runtime")
			agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Runtime profile capability agent")
			if _, err := testPool.Exec(ctx, `UPDATE agent SET runtime_config = $2::jsonb WHERE id = $1`,
				agentID, `{"harness_profile":"`+tc.profile+`"}`); err != nil {
				t.Fatalf("set profile: %v", err)
			}
			seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)
			task, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, parseUUID(runtimeID))
			if err != nil || task == nil {
				t.Fatalf("claim task: %v %+v", err, task)
			}
			runtime, err := testHandler.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
				ID: parseUUID(runtimeID), WorkspaceID: parseUUID(testWorkspaceID),
			})
			if err != nil {
				t.Fatalf("load runtime: %v", err)
			}
			req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil)
			if tc.capability != "" {
				req.Header.Set("X-Client-Capabilities", tc.capability)
			}
			resp, _, _, _, _, failure := testHandler.buildClaimedTaskResponse(req, task, runtime, runtimeID, testWorkspaceID)
			if !tc.wantFail {
				if failure != nil {
					t.Fatalf("unexpected failure: %+v", failure)
				}
				if resp.Agent == nil || resp.Agent.HarnessProfile != tc.profile {
					t.Fatalf("agent profile = %+v, want %q", resp.Agent, tc.profile)
				}
				return
			}
			if failure == nil {
				t.Fatalf("managed task handed to a daemon without runtime-profile capability: %+v", resp.Agent)
			}
			if !failure.settled || failure.outcome != "error_harness_profile_unsupported" {
				t.Fatalf("failure = %+v, want settled error_harness_profile_unsupported", failure)
			}
			var status, reason, msg string
			if err := testPool.QueryRow(ctx, `SELECT status, COALESCE(failure_reason,''), COALESCE(error,'') FROM agent_task_queue WHERE id = $1`, uuidToString(task.ID)).Scan(&status, &reason, &msg); err != nil {
				t.Fatalf("read task: %v", err)
			}
			if status != "failed" || !strings.Contains(msg, "runtime profile") {
				t.Fatalf("task status=%q reason=%q error=%q, want failed with a runtime-profile message", status, reason, msg)
			}
		})
	}
}
