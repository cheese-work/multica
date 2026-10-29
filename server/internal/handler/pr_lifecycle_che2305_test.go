package handler

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-2298 c1: a merged or closed PR stops the work that only serves it
// through the shared stop gate; draft and ready are notices to a live run.

var c1Counter atomic.Int32

type c1World struct {
	secret         string
	installationID int64
	repo           string
	number         int32
	prID           string
	issueID        string
	agentID        string
	runtimeID      string
}

func newC1World(t *testing.T) c1World {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	n := c1Counter.Add(1)
	w := c1World{
		secret:         "c1-secret",
		installationID: 77000000 + int64(n),
		repo:           fmt.Sprintf("c1repo%d", n),
		number:         int32(990000 + n),
	}
	t.Setenv("GITHUB_WEBHOOK_SECRET", w.secret)
	ctx := context.Background()
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID: parseUUID(testWorkspaceID), InstallationID: w.installationID, AccountLogin: "c1-acct", AccountType: "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}
	w.runtimeID = dbfx.Runtime(t, "c1-runtime", testutil.Cols{"provider": "codex"})
	w.agentID = dbfx.Agent(t, "C1 agent", w.runtimeID)
	w.issueID = dbfx.Issue(t, "c1 issue", testutil.Cols{"status": "in_progress"})
	dbfx.QueryRow(t, `
		INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha)
		VALUES ($1, $2, 'acme', $3, $4, 'c1 pr', 'open', 'https://example.test/pr', now(), now(), 'c1head')
		RETURNING id::text`, testWorkspaceID, w.installationID, w.repo, w.number).Scan(&w.prID)
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id, linked_by_type) VALUES ($1, $2, 'member')`, w.issueID, w.prID)
	t.Cleanup(func() {
		pool := context.Background()
		testPool.Exec(pool, `DELETE FROM task_pr_event_delivery WHERE issue_id = $1`, w.issueID)
		testPool.Exec(pool, `DELETE FROM issue_pr_event WHERE pull_request_id = $1`, w.prID)
		testPool.Exec(pool, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, w.issueID)
		testPool.Exec(pool, `DELETE FROM issue_wakeup WHERE issue_id = $1`, w.issueID)
		testPool.Exec(pool, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, w.prID)
		testPool.Exec(pool, `DELETE FROM github_pull_request WHERE id = $1`, w.prID)
		testPool.Exec(pool, `DELETE FROM github_installation WHERE installation_id = $1`, w.installationID)
	})
	return w
}

// fire sends one pull_request webhook for the world's PR.
func (w c1World) fire(t *testing.T, action, state string, merged, draft bool) {
	t.Helper()
	body := map[string]any{
		"action": action,
		"pull_request": map[string]any{
			"number": w.number, "html_url": "https://github.com/acme/" + w.repo + "/pull/1",
			"title": "c1", "body": "", "state": state, "draft": draft, "merged": merged,
			"merged_at": "2026-10-29T00:00:00Z", "closed_at": "2026-10-29T00:00:00Z",
			"created_at": "2026-10-28T00:00:00Z", "updated_at": time.Now().UTC().Add(time.Hour + time.Duration(c1Counter.Add(1))*time.Second).Format(time.RFC3339),
			"head": map[string]any{"ref": "c1/branch", "sha": "c1head", "repo": githubRepositoryForTest("acme", w.repo)},
			"user": map[string]any{"login": "octocat"},
		},
		"repository":   map[string]any{"id": 424242, "name": w.repo, "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": w.installationID},
	}
	postSignedGitHubWebhook(t, w.secret, body, "")
}

// prWakeRun plants a run started by a PR wakeup receipt for the world's PR.
func (w c1World) prWakeRun(t *testing.T, status, eventType, suffix string, over ...testutil.Cols) string {
	t.Helper()
	var wakeupID string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM issue_wakeup WHERE issue_id = $1 AND system_rule = 'pr_checks_failed'`, w.issueID).Scan(&wakeupID); err != nil {
		dbfx.QueryRow(t, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, instruction, kind, mode, event_types, system_rule)
			VALUES (gen_random_uuid(), $1, $2, 'pr', 'event', 'continuous', ARRAY[]::text[], 'pr_checks_failed')
			RETURNING id::text`, testWorkspaceID, w.issueID).Scan(&wakeupID)
	}
	cols := testutil.Cols{"issue_id": w.issueID, "runtime_id": w.runtimeID, "status": status}
	if status == "running" {
		cols["dispatched_at"] = testutil.Raw("now() - interval '5 minutes'")
		cols["started_at"] = testutil.Raw("now() - interval '4 minutes'")
	}
	for _, o := range over {
		for k, v := range o {
			cols[k] = v
		}
	}
	taskID := dbfx.Task(t, w.agentID, cols)
	dbfx.Exec(t, `INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload, task_id)
		VALUES (gen_random_uuid(), $1, 1, $2, $3, '{}'::jsonb, $4)`,
		wakeupID, fmt.Sprintf("github:acme/%s#%d%s", w.repo, w.number, suffix), eventType, taskID)
	return taskID
}

func (w c1World) addEvent(t *testing.T, kind string) string {
	t.Helper()
	var id string
	dbfx.QueryRow(t, `
		INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key)
		VALUES ($1, $2, $3, $4, 'c1head', 'base-1', now() - interval '1 hour', $4 || '|c1head|old')
		RETURNING id::text`, testWorkspaceID, w.issueID, w.prID, kind).Scan(&id)
	return id
}

func taskState(t *testing.T, taskID string) (status, reason string) {
	t.Helper()
	var r *string
	dbfx.QueryRow(t, `SELECT status, failure_reason FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status, &r)
	if r != nil {
		reason = *r
	}
	return status, reason
}

func TestPRMergedOrClosedStopsPROnlyWorkViaGate(t *testing.T) {
	for _, tc := range []struct {
		name, action, state, reason string
		merged                      bool
		kind                        string
	}{
		{"merged", "closed", "closed", "pr_merged", true, "merged"},
		{"closed", "closed", "closed", "pr_closed", false, "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newC1World(t)
			// The author run serves the issue, not only the PR; a run started by
			// a PR wake serves only the PR.
			author := dbfx.Task(t, w.agentID, testutil.Cols{
				"issue_id": w.issueID, "runtime_id": w.runtimeID, "status": "running",
				"dispatched_at": testutil.Raw("now() - interval '30 minutes'"), "started_at": testutil.Raw("now() - interval '29 minutes'"),
			})
			prRun := w.prWakeRun(t, "running", "pr.checks_failed", ":c1head")
			queuedPRRun := w.prWakeRun(t, "queued", "pr.mergeability", ":mergeability:c1head:main:dirty")
			conflict := w.addEvent(t, "conflict")

			w.fire(t, tc.action, tc.state, tc.merged, false)

			if s, r := taskState(t, prRun); s != "cancelled" || r != tc.reason {
				t.Fatalf("PR-only running run = %s/%q, want cancelled/%q", s, r, tc.reason)
			}
			if s, r := taskState(t, queuedPRRun); s != "cancelled" || r != tc.reason {
				t.Fatalf("PR-only queued run = %s/%q, want cancelled/%q", s, r, tc.reason)
			}
			if s, _ := taskState(t, author); s != "running" {
				t.Fatalf("author run = %s, want running: it serves more than the PR", s)
			}
			var eventStatus string
			dbfx.QueryRow(t, `SELECT status FROM issue_pr_event WHERE id = $1`, conflict).Scan(&eventStatus)
			if eventStatus != "superseded" {
				t.Fatalf("pending conflict event = %q, want superseded: nothing to fix on a %s PR", eventStatus, tc.name)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM issue_pr_event WHERE pull_request_id = $1 AND kind = $2`, w.prID, tc.kind); n != 1 {
				t.Fatalf("%s events recorded = %d, want 1", tc.kind, n)
			}

			// The gate holds until the daemon proves the runner stopped.
			stopGateClosedFor(t, w.issueID, prRun)

			// Cleanup still runs: the cancel-ack carries the preserved work dir
			// and opens the gate.
			var runtimeID string
			dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent_task_queue WHERE id = $1`, prRun).Scan(&runtimeID)
			ackCancelled(t, prRun, map[string]any{
				"cancel_ack_proof": auth.CancelAckProof(prRun, runtimeID),
				"durable_work_dir": "/tmp/c1-preserved",
			})
			var dir *string
			dbfx.QueryRow(t, `SELECT durable_work_dir FROM agent_task_queue WHERE id = $1`, prRun).Scan(&dir)
			if dir == nil || *dir != "/tmp/c1-preserved" {
				t.Fatalf("durable_work_dir after ack = %v, want /tmp/c1-preserved", dir)
			}
			stopGateOpen(t, w.issueID)
		})
	}
}

func TestPRMergedKeepsThePostMergeWakeRun(t *testing.T) {
	w := newC1World(t)
	postMerge := w.prWakeRun(t, "queued", "pr.merged", "")
	w.fire(t, "closed", "closed", true, false)
	if s, _ := taskState(t, postMerge); s != "queued" {
		t.Fatalf("pr.merged wake run = %s, want queued: it is the work the merge asks for", s)
	}
}

func TestPRDraftAndReadyAreNoticesOnly(t *testing.T) {
	w := newC1World(t)
	taskID := dbfx.Task(t, w.agentID, testutil.Cols{
		"issue_id": w.issueID, "runtime_id": w.runtimeID, "status": "running",
		"dispatched_at": testutil.Raw("now()"), "started_at": testutil.Raw("now()"),
	})
	dbfx.Exec(t, `INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, $4)`,
		taskID, testWorkspaceID, w.issueID, protocol.DaemonCapabilityTaskSupplementPREventV1)
	dbfx.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id = $1`, taskID)
	before := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, w.issueID)

	w.fire(t, "converted_to_draft", "open", false, true)
	w.fire(t, "ready_for_review", "open", false, false)

	var kinds []string
	rows, err := testPool.Query(t.Context(), `
		SELECT e.kind FROM task_pr_event_delivery d JOIN issue_pr_event e ON e.id = d.event_id
		WHERE d.task_id = $1 AND d.status = 'pending' ORDER BY e.observed_at, e.created_at`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	if len(kinds) != 2 || kinds[0] != "draft" || kinds[1] != "ready" {
		t.Fatalf("pending notices for the live run = %v, want [draft ready]", kinds)
	}
	if after := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, w.issueID); after != before {
		t.Fatalf("tasks on the issue = %d, want %d: draft/ready must not start a run", after, before)
	}
	if s, _ := taskState(t, taskID); s != "running" {
		t.Fatalf("live run = %s, want running", s)
	}
}

// A cancel that failed after the mirror committed leaves the PR merged with its
// run still running; the redelivered payload must finish the stop.
func TestPRLifecycleRetryFinishesUnfinishedStop(t *testing.T) {
	w := newC1World(t)
	prRun := w.prWakeRun(t, "running", "pr.checks_failed", "")

	w.fire(t, "closed", "closed", true, false)
	if s, r := taskState(t, prRun); s != "cancelled" || r != "pr_merged" {
		t.Fatalf("first delivery = %s/%q, want cancelled/pr_merged", s, r)
	}

	// The failed first attempt, as the retry finds it: mirror merged, run alive.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', failure_reason = NULL, completed_at = NULL WHERE id = $1`, prRun)
	w.fire(t, "closed", "closed", true, false)
	if s, r := taskState(t, prRun); s != "cancelled" || r != "pr_merged" {
		t.Fatalf("redelivery = %s/%q, want cancelled/pr_merged: the unfinished stop must be retried", s, r)
	}
}

// The unconfirmed-cleanup ack rewrites the cancelled run to failed. The gate
// must stay closed for a merged or closed PR whatever step the issue moves to.
func TestPRLifecycleHoldSurvivesRewriteAndStepChange(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		merged       bool
	}{{"merged", "pr_merged", true}, {"closed", "pr_closed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newC1World(t)
			prRun := w.prWakeRun(t, "running", "pr.checks_failed", "")
			w.fire(t, "closed", "closed", tc.merged, false)
			if s, r := taskState(t, prRun); s != "cancelled" || r != tc.reason {
				t.Fatalf("run = %s/%q, want cancelled/%s", s, r, tc.reason)
			}

			var runtimeID string
			dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent_task_queue WHERE id = $1`, prRun).Scan(&runtimeID)
			ackCancelled(t, prRun, map[string]any{
				"cancel_ack_proof": auth.CancelAckProof(prRun, runtimeID),
				"failure_reason":   "cleanup_unconfirmed",
				"error_message":    "cleanup failed",
			})
			if s, r := taskState(t, prRun); s != "failed" || r != "cleanup_unconfirmed" {
				t.Fatalf("run after unconfirmed ack = %s/%q, want failed/cleanup_unconfirmed", s, r)
			}

			dbfx.Exec(t, `UPDATE issue SET status = 'todo' WHERE id = $1`, w.issueID)
			stopGateClosedFor(t, w.issueID, prRun)
		})
	}
}

// A draft notice stored while no capable run was live is delivered by the next
// delivery for the PR, even though that delivery is not a state transition.
func TestPRLifecycleRetryDeliversStoredNotice(t *testing.T) {
	w := newC1World(t)
	w.fire(t, "converted_to_draft", "open", false, true)
	taskID := dbfx.Task(t, w.agentID, testutil.Cols{
		"issue_id": w.issueID, "runtime_id": w.runtimeID, "status": "running",
		"dispatched_at": testutil.Raw("now()"), "started_at": testutil.Raw("now()"),
	})
	dbfx.Exec(t, `INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, $4)`,
		taskID, testWorkspaceID, w.issueID, protocol.DaemonCapabilityTaskSupplementPREventV1)
	dbfx.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id = $1`, taskID)

	w.fire(t, "synchronize", "open", false, true)
	if n := dbfx.Count(t, `SELECT count(*) FROM task_pr_event_delivery d JOIN issue_pr_event e ON e.id = d.event_id WHERE d.task_id = $1 AND e.kind = 'draft' AND d.status = 'pending'`, taskID); n != 1 {
		t.Fatalf("pending draft notices for the live run = %d, want 1", n)
	}
}
