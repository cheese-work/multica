package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5094 (CHE-4678 S9 P5): a settled script run carries audit fields in
// autopilot_run.result.script, and a failed task-token deletion is recorded.

type scriptAudit struct {
	Name       string `json:"name"`
	ArgvSHA256 string `json:"argv_sha256"`
	ExitCode   *int   `json:"exit_code"`
	DurationMs *int64 `json:"duration_ms"`
}

type auditedRun struct {
	Script           *scriptAudit `json:"script"`
	TokenRevokeError bool         `json:"token_revoke_failed"`
}

func (f scriptFixture) runResult(t *testing.T) (status string, failure *string, res auditedRun) {
	t.Helper()
	var raw []byte
	f.QueryRow(t, `SELECT status, failure_reason, result FROM autopilot_run WHERE id = $1`, util.UUIDToString(f.run.ID)).Scan(&status, &failure, &raw)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("run result %s: %v", raw, err)
		}
	}
	return status, failure, res
}

type scriptFixture struct {
	principalFixture
	run  db.AutopilotRun
	task db.AgentTaskQueue
}

func newScriptFixture(t *testing.T) scriptFixture {
	t.Helper()
	fx, _, autopilotID, triggerID := modelGatedFixture(t, "script")
	fx.Exec(t, `UPDATE autopilot SET script_config = '{"name":"sweep-reconcile","args":["--once","--limit=5"]}'::jsonb WHERE id = $1`, autopilotID)
	run := fx.dispatch(t, autopilotID, triggerID)
	task, err := fx.q.GetAutopilotTaskByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("task for run: %v", err)
	}
	return scriptFixture{principalFixture: fx, run: *run, task: task}
}

func TestScriptRunAuditFields(t *testing.T) {
	wantHash := sha256.Sum256([]byte("--once\x00--limit=5"))
	exit0, exit7 := 0, 7
	for _, tc := range []struct {
		name       string
		status     string
		taskErr    string
		runStatus  string
		wantExit   *int
		wantFailed string
	}{
		{"completed", "completed", "", "completed", &exit0, ""},
		{"nonzero exit", "failed", "script_failed: exit code 7\nlast line", "failed", &exit7, "script_failed"},
		{"unknown script fails closed", "failed", "script_not_allowlisted: script name is not allowlisted", "failed", nil, "script_not_allowlisted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScriptFixture(t)
			f.svc.RecordScriptClaim(context.Background(), f.run.ID, "sweep-reconcile", []string{"--once", "--limit=5"})
			f.task.Status = tc.status
			f.task.Error = pgtype.Text{String: tc.taskErr, Valid: tc.taskErr != ""}
			f.task.FailureReason = pgtype.Text{String: tc.wantFailed, Valid: tc.wantFailed != ""}
			start := time.Now()
			f.task.StartedAt = pgtype.Timestamptz{Time: start, Valid: true}
			f.task.CompletedAt = pgtype.Timestamptz{Time: start.Add(1500 * time.Millisecond), Valid: true}
			f.svc.SyncRunFromTask(context.Background(), f.task)

			status, failure, res := f.runResult(t)
			if status != tc.runStatus {
				t.Fatalf("run status = %q, want %q", status, tc.runStatus)
			}
			if tc.wantFailed != "" && (failure == nil || !strings.Contains(*failure, tc.wantFailed)) {
				t.Errorf("failure_reason = %v, want it to name %q", failure, tc.wantFailed)
			}
			a := res.Script
			if a == nil {
				t.Fatal("result.script audit missing")
			}
			if a.Name != "sweep-reconcile" || a.ArgvSHA256 != hex.EncodeToString(wantHash[:]) {
				t.Errorf("audit name/hash = %q/%q", a.Name, a.ArgvSHA256)
			}
			if (a.ExitCode == nil) != (tc.wantExit == nil) || (a.ExitCode != nil && *a.ExitCode != *tc.wantExit) {
				t.Errorf("exit_code = %v, want %v", a.ExitCode, tc.wantExit)
			}
			if a.DurationMs == nil || *a.DurationMs != 1500 {
				t.Errorf("duration_ms = %v, want 1500", a.DurationMs)
			}
			if strings.Contains(a.ArgvSHA256, "limit") {
				t.Error("audit must store the argv hash, never the argv")
			}
		})
	}
}

func (f scriptFixture) addToken(t *testing.T) {
	t.Helper()
	if _, err := f.q.CreateTaskToken(context.Background(), db.CreateTaskTokenParams{
		TokenHash: "audit-test-hash-" + util.UUIDToString(f.task.ID), TaskID: f.task.ID, AgentID: f.task.AgentID,
		WorkspaceID: util.MustParseUUID(f.WorkspaceID), UserID: util.MustParseUUID(f.UserID), ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
}

func (f scriptFixture) tokens(t *testing.T) int {
	return f.Count(t, `SELECT count(*) FROM task_token WHERE task_id = $1`, util.UUIDToString(f.task.ID))
}

func TestRevokeTaskTokensDeletesAtTaskEnd(t *testing.T) {
	f := newScriptFixture(t)
	f.addToken(t)
	f.svc.TaskSvc.RevokeTaskTokens(context.Background(), f.task)
	if n := f.tokens(t); n != 0 {
		t.Fatalf("tokens after revoke = %d, want 0", n)
	}
	if _, _, res := f.runResult(t); res.TokenRevokeError {
		t.Error("clean deletion must not be recorded as a failure")
	}
}

func TestRevokeTaskTokensFailureIsRecorded(t *testing.T) {
	f := newScriptFixture(t)
	f.addToken(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the delete cannot run; the record still must
	f.svc.TaskSvc.RevokeTaskTokens(ctx, f.task)
	if n := f.tokens(t); n != 1 {
		t.Fatalf("tokens = %d, want the row to stay until expiry", n)
	}
	if _, _, res := f.runResult(t); !res.TokenRevokeError {
		t.Fatal("failed deletion not recorded on the run result")
	}
}

// The audit names what the daemon was told to run at claim, not the autopilot
// config at settlement. Editing the autopilot between claim and settlement
// must not rewrite the record.
func TestScriptAuditBindsToClaimedScript(t *testing.T) {
	f := newScriptFixture(t)
	f.svc.RecordScriptClaim(context.Background(), f.run.ID, "sweep-reconcile", []string{"--once", "--limit=5"})
	f.Exec(t, `UPDATE autopilot SET script_config = '{"name":"other-script","args":["--changed"]}'::jsonb WHERE id = $1`, util.UUIDToString(f.run.AutopilotID))

	f.task.Status = "completed"
	f.svc.SyncRunFromTask(context.Background(), f.task)

	_, _, res := f.runResult(t)
	want := sha256.Sum256([]byte("--once\x00--limit=5"))
	if res.Script == nil {
		t.Fatal("result.script audit missing")
	}
	if res.Script.Name != "sweep-reconcile" || res.Script.ArgvSHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("audit follows the live config: name=%q hash=%q", res.Script.Name, res.Script.ArgvSHA256)
	}
}

// With no recorded claim the audit must not fall back to the live config.
func TestScriptAuditWithoutClaimDoesNotUseLiveConfig(t *testing.T) {
	f := newScriptFixture(t)
	f.task.Status = "completed"
	f.svc.SyncRunFromTask(context.Background(), f.task)
	_, _, res := f.runResult(t)
	if res.Script == nil {
		t.Fatal("result.script audit missing")
	}
	if res.Script.Name != "" || res.Script.ArgvSHA256 != "" {
		t.Errorf("unclaimed run audit leaked the live config: name=%q hash=%q", res.Script.Name, res.Script.ArgvSHA256)
	}
}

// A mode change between claim and settlement must not drop the audit: the
// recorded claim, not the live execution mode, identifies a script run.
func TestScriptAuditSurvivesModeChangeAfterClaim(t *testing.T) {
	f := newScriptFixture(t)
	f.svc.RecordScriptClaim(context.Background(), f.run.ID, "sweep-reconcile", []string{"--once", "--limit=5"})
	f.Exec(t, `UPDATE autopilot SET execution_mode = 'run_only' WHERE id = $1`, util.UUIDToString(f.run.AutopilotID))

	f.task.Status = "completed"
	f.svc.SyncRunFromTask(context.Background(), f.task)

	_, _, res := f.runResult(t)
	if res.Script == nil {
		t.Fatal("result.script audit lost after execution mode change")
	}
	if res.Script.Name != "sweep-reconcile" || res.Script.ExitCode == nil || *res.Script.ExitCode != 0 {
		t.Errorf("audit incomplete: %+v", res.Script)
	}
}
