package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDispatchBlockedReentrySkipsOnlyUnchangedNonMemberTransition(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)

	for _, tc := range []struct {
		name       string
		actor      string
		mutate     func(*testing.T, string)
		mode       string
		wantQueued bool
		wantSkip   bool
	}{
		{name: "unchanged agent transition", actor: "agent", wantSkip: true},
		{name: "reason changed", actor: "agent", mutate: func(t *testing.T, id string) {
			dbfx.Exec(t, `UPDATE issue SET metadata = '{"blocked_reason":"changed"}' WHERE id=$1`, id)
		}, wantQueued: true},
		{name: "PR head changed", actor: "agent", mutate: func(t *testing.T, id string) { changeBlockedDispatchPR(t, id, "head-2", "FAILURE") }, wantQueued: true},
		{name: "required checks changed", actor: "agent", mutate: func(t *testing.T, id string) { changeBlockedDispatchPR(t, id, "head-1", "SUCCESS") }, wantQueued: true},
		{name: "qualifying comment added", actor: "agent", mutate: func(t *testing.T, id string) {
			dbfx.Exec(t, `INSERT INTO comment (workspace_id,issue_id,author_type,author_id,content) VALUES ($1,$2,'member',$3,'new blocker context')`, testWorkspaceID, id, testUserID)
		}, wantQueued: true},
		{name: "workspace mode warn", actor: "agent", mode: "warn", wantQueued: true},
		{name: "workspace mode off", actor: "agent", mode: "off", wantQueued: true},
		{name: "member transition", actor: "member", wantQueued: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var originalSettings []byte
			dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&originalSettings)
			t.Cleanup(func() {
				dbfx.Exec(t, `UPDATE workspace SET settings=$1 WHERE id=$2`, originalSettings, testWorkspaceID)
			})
			issueID := dbfx.Issue(t, "blocked dispatch", testutil.Cols{
				"status": "blocked", "assignee_type": "agent", "assignee_id": agentID,
				"metadata": testutil.Raw(`'{"blocked_reason":"waiting"}'::jsonb`),
			})
			if tc.mode != "" {
				settings, _ := json.Marshal(map[string]any{"blocked_dispatch_dedup": map[string]string{"mode": tc.mode}})
				dbfx.Exec(t, `UPDATE workspace SET settings=$1 WHERE id=$2`, settings, testWorkspaceID)
			}
			t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id=$1`, issueID) })
			issueUUID := parseUUID(issueID)
			tofuID := parseUUID(agentID)
			inputs, err := testHandler.Queries.GetBlockedDispatchFingerprint(context.Background(), db.GetBlockedDispatchFingerprintParams{IssueID: issueUUID, TofuID: tofuID})
			if err != nil {
				t.Fatal(err)
			}
			fingerprint := sha256.Sum256([]byte(inputs))
			encoded, _ := json.Marshal(map[string]string{"blocked_dispatch_fingerprint": hex.EncodeToString(fingerprint[:])})
			dbfx.Task(t, agentID, testutil.Cols{
				"issue_id": issueID, "runtime_id": runtimeID, "status": "completed",
				"completed_at": testutil.Raw("now() - interval '1 minute'"), "context": testutil.Raw("'" + string(encoded) + "'::jsonb"),
			})
			if tc.mutate != nil {
				tc.mutate(t, issueID)
			}
			loaded, err := testHandler.Queries.GetIssue(context.Background(), issueUUID)
			if err != nil {
				t.Fatal(err)
			}

			started := testHandler.dispatchIssueRun(context.Background(), loaded, service.IssueRunTrigger{
				IssueID: issueUUID, AgentID: tofuID, AssigneeType: "agent", BlockedEntry: true,
			}, tc.actor, testUserID, "")
			if started != tc.wantQueued {
				t.Fatalf("dispatch started = %v, want %v", started, tc.wantQueued)
			}
			if got := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status IN ('queued','dispatched')`, issueID); got != boolCount(tc.wantQueued) {
				t.Fatalf("pending tasks = %d, want %d", got, boolCount(tc.wantQueued))
			}
			if got := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_skipped'`, issueID); got != boolCount(tc.wantSkip) {
				t.Fatalf("wakeup_skipped activities = %d, want %d", got, boolCount(tc.wantSkip))
			}
			if tc.wantSkip {
				var details string
				dbfx.QueryRow(t, `SELECT details::text FROM activity_log WHERE issue_id=$1 AND action='wakeup_skipped'`, issueID).Scan(&details)
				var data map[string]any
				if err := json.Unmarshal([]byte(details), &data); err != nil {
					t.Fatal(err)
				}
				if data["actor_type"] != "agent" || data["target_type"] != "agent" || data["target_id"] != agentID || data["reason"] != "unchanged inputs" {
					t.Errorf("activity details = %v", data)
				}
				if events, ok := data["events"].([]any); !ok || len(events) != 1 || events[0] != "issue.status_changed" {
					t.Errorf("activity events = %v", data["events"])
				}
			}
		})
	}
}

func changeBlockedDispatchPR(t *testing.T, issueID, headSHA, checks string) {
	t.Helper()
	var prID string
	dbfx.QueryRow(t, `INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, api_merge_state_status, checks_rollup_state)
	VALUES ($1, 1, 'acme', 'widget', 157, 'test', 'open', 'https://github.com/acme/widget/pull/157', now(), now(), 'head-1', 'CLEAN', 'FAILURE') RETURNING id`, testWorkspaceID).Scan(&prID)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM github_pull_request WHERE id=$1`, prID) })
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id,pull_request_id) VALUES ($1,$2)`, issueID, prID)
	dbfx.Exec(t, `UPDATE github_pull_request SET head_sha=$2,checks_rollup_state=$3 WHERE id=$1`, prID, headSHA, checks)
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

// A direct run that loses the pending-task unique index to another run counts
// as delivery only when that other run has not started: a queued run reads the
// issue when it is claimed, and a dispatched one read it earlier. The caller
// settles the handed agent's receipts only on delivery, so a dispatched run
// that holds the slot leaves the event wakeup as the change's delivery.
func TestDispatchIssueRunCountsOnlyAnUnstartedDuplicateAsDelivery(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	cases := []struct {
		name     string
		existing string
		want     bool
	}{
		{"no run holds the slot", "", true},
		{"a queued run holds the slot", "queued", true},
		{"a dispatched run holds the slot", "dispatched", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issueID := dbfx.Issue(t, "duplicate dispatch", testutil.Cols{
				"status": "todo", "assignee_type": "agent", "assignee_id": agentID,
			})
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
			})
			if tc.existing != "" {
				cols := testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": tc.existing}
				if tc.existing == "dispatched" {
					cols["dispatched_at"] = testutil.Raw("now()")
				}
				dbfx.Task(t, agentID, cols)
			}
			issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
			if err != nil {
				t.Fatal(err)
			}

			got := testHandler.dispatchIssueRun(context.Background(), issue,
				service.IssueRunTrigger{IssueID: issue.ID, AgentID: parseUUID(agentID), AssigneeType: "agent"},
				"member", testUserID, "")

			if got != tc.want {
				t.Fatalf("dispatchIssueRun = %v, want %v", got, tc.want)
			}
		})
	}
}

// The run that holds the slot must carry THIS write: assignment-level, and
// stamped for the step the issue is in now. A queued run left over from a step
// the issue has left, or one without a step while the current step hands off,
// reads the issue at claim but not the step's brief and permissions, so it is
// not delivery.
func TestDispatchIssueRunDuplicateMustMatchTheCurrentStep(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	cases := []struct {
		name string
		step any
		want bool
	}{
		{"queued run stamped for the current step", "in_review", true},
		{"queued run stamped for a step the issue left", "in_progress", false},
		{"queued run without a step while the step hands off", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projectID := createWorkflowTestProject(t, "Duplicate step project")
			setColumnHandoff(t, projectID, "in_review", handToAgent(agentID, "Review it.")).Want(http.StatusOK)
			issueID := dbfx.Issue(t, "duplicate step", testutil.Cols{
				"project_id": projectID, "status": "in_review", "assignee_type": "agent", "assignee_id": agentID,
			})
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
			})
			cols := testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "queued"}
			if tc.step != nil {
				cols["workflow_step"] = tc.step
			}
			dbfx.Task(t, agentID, cols)
			issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
			if err != nil {
				t.Fatal(err)
			}

			got := testHandler.dispatchIssueRun(context.Background(), issue,
				service.IssueRunTrigger{IssueID: issue.ID, AgentID: parseUUID(agentID), AssigneeType: "agent"},
				"member", testUserID, "")

			if got != tc.want {
				t.Fatalf("dispatchIssueRun = %v, want %v", got, tc.want)
			}
		})
	}
}

// A media-gated run that the issue's earlier step left behind still holds the
// unique pending slot. The write that enters the next step must cancel it and
// queue the run for the new step, or the replacement deduplicates against the
// stale run and the change reaches the agent through neither.
func TestStaleMediaDeferredStepRunIsReplacedWithReceiptsKept(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	for _, writer := range issueWriters {
		t.Run(writer.name, func(t *testing.T) {
			projectID := createWorkflowTestProject(t, "Stale deferred project")
			setColumnHandoff(t, projectID, "in_review", handToAgent(agentID, "Review it.")).Want(http.StatusOK)
			issueID := dbfx.Issue(t, "stale deferred", testutil.Cols{"project_id": projectID, "status": "in_progress"})
			stale := dbfx.Task(t, agentID, testutil.Cols{
				"issue_id": issueID, "runtime_id": runtimeID, "status": "deferred", "workflow_step": "in_progress",
				"context": testutil.Raw(`'{"channel_issue_media_pending":"true"}'::jsonb`),
			})
			wakeupID := subscribeToIssueEvent(t, issueID, agentID, "issue.status_changed")

			writer.write(t, issueID, map[string]any{"status": "in_review"}, nil).Want(http.StatusOK)

			var staleStatus string
			dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, stale).Scan(&staleStatus)
			if staleStatus != "cancelled" {
				t.Fatalf("the stale media-deferred run = %q, want cancelled", staleStatus)
			}
			if steps := pendingStepRuns(t, issueID, agentID); len(steps) != 1 || steps[0] != "in_review" {
				t.Fatalf("pending runs = %v, want one stamped in_review", steps)
			}
			// The executable run carries the change, so the receipt is handled.
			if got := pendingEventReceipts(t, wakeupID); got != 0 {
				t.Fatalf("pending receipts = %d, want 0 once the new-step run exists", got)
			}
		})
	}
}
