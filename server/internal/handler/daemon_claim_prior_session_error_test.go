package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-1710: a resumed Antigravity conversation can keep an old result error
// after the current turn finished. The claim hands the daemon the recorded
// error of the session it resumes as positive evidence, and only for that
// session and agent.

const priorSessionErrorText = "agent executor error: NOT_FOUND (code 404): Requested entity was not found."

func TestClaimTask_IssueOffersPriorSessionError(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	agentID, runtimeID, daemonID := createRuntimeGuardAgent(t, ctx)
	issueID := dbfx.Issue(t, "prior session error fixture", testutil.Cols{"status": "in_progress", "number": 87100})

	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at, completed_at, session_id, work_dir, error)
		VALUES ($1, $2, $3, 'failed', 0, now() - interval '2 minutes', now() - interval '1 minutes', 'err-session', '/tmp/err-workdir', $4)
	`, agentID, runtimeID, issueID, priorSessionErrorText)
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'queued', 0)
	`, agentID, runtimeID, issueID)

	task := claimTaskForRuntimeGuard(t, runtimeID, daemonID)
	if task.PriorSessionID != "err-session" || task.PriorSessionError != priorSessionErrorText {
		t.Fatalf("PriorSessionID = %q, PriorSessionError = %q, want err-session and the recorded error", task.PriorSessionID, task.PriorSessionError)
	}
}

func TestClaimTask_ChatOffersPriorSessionErrorEvenAfterCleanTurn(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	agentID, runtimeID, daemonID := createRuntimeGuardAgent(t, ctx)
	chatSessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "prior session error chat"})

	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority, started_at, completed_at, session_id, work_dir, error)
		VALUES ($1, $2, $3, 'failed', 0, now() - interval '5 minutes', now() - interval '4 minutes', 'err-chat-session', '/tmp/err-chat-workdir', $4)
	`, agentID, runtimeID, chatSessionID, priorSessionErrorText)
	// A later clean turn of the same session keeps the evidence reachable.
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority, started_at, completed_at, session_id, work_dir)
		VALUES ($1, $2, $3, 'completed', 0, now() - interval '2 minutes', now() - interval '1 minutes', 'err-chat-session', '/tmp/err-chat-workdir')
	`, agentID, runtimeID, chatSessionID)
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority)
		VALUES ($1, $2, $3, 'queued', 0)
	`, agentID, runtimeID, chatSessionID)

	task := claimTaskForRuntimeGuard(t, runtimeID, daemonID)
	if task.PriorSessionID != "err-chat-session" || task.PriorSessionError != priorSessionErrorText {
		t.Fatalf("PriorSessionID = %q, PriorSessionError = %q, want err-chat-session and the recorded error", task.PriorSessionID, task.PriorSessionError)
	}
}

func TestClaimTask_PriorSessionErrorStaysScopedToResumedSessionAndAgent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	agentID, runtimeID, daemonID := createRuntimeGuardAgent(t, ctx)
	var otherAgentID string
	dbfx.QueryRow(t, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
		VALUES ($1, 'Prior Session Error Other Agent', 'local', '{}'::jsonb, $2, 'workspace', 3, $3)
		RETURNING id
	`, testWorkspaceID, runtimeID, testUserID).Scan(&otherAgentID)
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent WHERE id = $1`, otherAgentID) })
	issueID := dbfx.Issue(t, "prior session error scope fixture", testutil.Cols{"status": "in_progress", "number": 87101})

	// Same session id recorded by another agent: not this agent's evidence.
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at, completed_at, session_id, error)
		VALUES ($1, $2, $3, 'failed', 0, now() - interval '9 minutes', now() - interval '8 minutes', 'scoped-session', $4)
	`, otherAgentID, runtimeID, issueID, priorSessionErrorText)
	// A different session of this agent failed, but the resumed one is clean.
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at, completed_at, session_id, error)
		VALUES ($1, $2, $3, 'failed', 0, now() - interval '6 minutes', now() - interval '5 minutes', 'older-session', $4)
	`, agentID, runtimeID, issueID, priorSessionErrorText)
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at, completed_at, session_id, work_dir)
		VALUES ($1, $2, $3, 'completed', 0, now() - interval '2 minutes', now() - interval '1 minutes', 'scoped-session', '/tmp/scoped-workdir')
	`, agentID, runtimeID, issueID)
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'queued', 0)
	`, agentID, runtimeID, issueID)

	task := claimTaskForRuntimeGuard(t, runtimeID, daemonID)
	if task.PriorSessionID != "scoped-session" || task.PriorSessionError != "" {
		t.Fatalf("PriorSessionID = %q, PriorSessionError = %q, want scoped-session and no evidence", task.PriorSessionID, task.PriorSessionError)
	}
}

func TestClaimTask_FreshSessionOffersNoPriorSessionError(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	agentID, runtimeID, daemonID := createRuntimeGuardAgent(t, ctx)
	issueID := dbfx.Issue(t, "prior session error fresh fixture", testutil.Cols{"status": "in_progress", "number": 87102})

	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at, completed_at, session_id, work_dir, error)
		VALUES ($1, $2, $3, 'failed', 0, now() - interval '2 minutes', now() - interval '1 minutes', 'fresh-err-session', '/tmp/fresh-workdir', $4)
	`, agentID, runtimeID, issueID, priorSessionErrorText)
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, force_fresh_session)
		VALUES ($1, $2, $3, 'queued', 0, true)
	`, agentID, runtimeID, issueID)

	task := claimTaskForRuntimeGuard(t, runtimeID, daemonID)
	if task.PriorSessionID != "" || task.PriorSessionError != "" {
		t.Fatalf("PriorSessionID = %q, PriorSessionError = %q, want neither on a forced-fresh session", task.PriorSessionID, task.PriorSessionError)
	}
}
