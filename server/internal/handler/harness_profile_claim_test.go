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
