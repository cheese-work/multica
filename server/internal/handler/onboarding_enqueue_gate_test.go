package handler

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The fork onboarding caller (onboarding_shim.go BootstrapOnboardingRuntime)
// dispatches its first issue through shouldEnqueueAgentTask. Upstream v0.10.0
// deleted that handler helper, which left the call undefined on the assembled
// tree (CHE-6929 P1). This pins the three decisions the caller relies on:
// backlog and triage park the issue silently, a ready agent dispatches, and a
// blocked runtime refuses with a durable notice on the issue.
func TestOnboardingEnqueueGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	var blockedRuntimeID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, last_seen_at
		)
		VALUES ($1, NULL, $2, 'cloud', $3, 'offline',  $4,
			'{"offline_reason":{"code":"not_executable","detail":"agent CLI is not executable"}}'::jsonb, $5, now())
		RETURNING id
	`, testWorkspaceID, "Onboarding Gate Blocked "+suffix, "onboarding_gate_blocked", "Blocked runtime", testUserID).Scan(&blockedRuntimeID); err != nil {
		t.Fatalf("insert blocked runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent WHERE runtime_id = $1`, blockedRuntimeID)
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, blockedRuntimeID)
	})

	newAgent := func(runtimeID, name string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent (
				workspace_id, name, description, runtime_mode, runtime_config,
				runtime_id, visibility, permission_mode, max_concurrent_tasks, owner_id
			)
			VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'workspace', 'public_to', 1, $4)
			RETURNING id
		`, testWorkspaceID, name, runtimeID, testUserID).Scan(&id); err != nil {
			t.Fatalf("insert agent %s: %v", name, err)
		}
		t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent WHERE id = $1`, id) })
		return id
	}
	readyAgent := newAgent(testRuntimeID, "Onboarding Gate Ready "+suffix)
	blockedAgent := newAgent(blockedRuntimeID, "Onboarding Gate Blocked "+suffix)

	// issueFor creates a real issue (the notice is a comment on it), assigns the
	// agent and reads it back as the row the caller would hold.
	issueFor := func(status, agentID string, triage bool) (string, func() bool) {
		id := createTestIssue(t, "onboarding gate "+status+" "+uuid.NewString()[:8], status, "high")
		t.Cleanup(func() { deleteTestIssue(t, id) })
		if _, err := testPool.Exec(ctx,
			`UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, id, agentID); err != nil {
			t.Fatalf("assign agent: %v", err)
		}
		if triage {
			if _, err := testPool.Exec(ctx, `UPDATE issue SET triage_state = 'pending' WHERE id = $1`, id); err != nil {
				t.Fatalf("set triage: %v", err)
			}
		}
		return id, func() bool {
			issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(id))
			if err != nil {
				t.Fatalf("read issue: %v", err)
			}
			return testHandler.shouldEnqueueAgentTask(ctx, issue)
		}
	}
	systemNotes := func(issueID string) int {
		var n int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, issueID).Scan(&n); err != nil {
			t.Fatalf("count system comments: %v", err)
		}
		return n
	}

	t.Run("ready agent dispatches", func(t *testing.T) {
		id, decide := issueFor("todo", readyAgent, false)
		if !decide() {
			t.Fatal("todo issue with a ready agent must enqueue")
		}
		if n := systemNotes(id); n != 0 {
			t.Fatalf("a dispatch left %d system notes, want 0", n)
		}
	})
	t.Run("backlog parks silently", func(t *testing.T) {
		id, decide := issueFor("backlog", blockedAgent, false)
		if decide() {
			t.Fatal("backlog issue must not enqueue")
		}
		if n := systemNotes(id); n != 0 {
			t.Fatalf("a parked issue got %d system notes, want 0", n)
		}
	})
	t.Run("triage parks silently", func(t *testing.T) {
		id, decide := issueFor("todo", readyAgent, true)
		if decide() {
			t.Fatal("triage issue must not enqueue")
		}
		if n := systemNotes(id); n != 0 {
			t.Fatalf("a triage issue got %d system notes, want 0", n)
		}
	})
	t.Run("blocked runtime refuses and explains", func(t *testing.T) {
		id, decide := issueFor("todo", blockedAgent, false)
		if decide() {
			t.Fatal("issue whose agent runtime is unusable must not enqueue")
		}
		if n := systemNotes(id); n != 1 {
			t.Fatalf("blocked runtime left %d system notes on the issue, want 1", n)
		}
	})
}
