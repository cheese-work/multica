package service

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-4575 (CHE-2804 part 1): a terminal gateway-class failure keeps its
// timeline failure comment but drops the agent wake: no agent mention link and
// no new run for any agent.
var gatewayClassFailures = []struct {
	name   string
	reason string
	errMsg string
}{
	{"transport", "agent_error.provider_network", "API Error: Connection closed mid-response"},
	{"capacity", "agent_error.provider_capacity_or_rate_limit", "API Error: 429 rate limited"},
	{"server error", "agent_error.provider_server_error", "API Error: 503 service unavailable"},
	{"quota", "agent_error.provider_quota_limit", "API Error: 402 insufficient_balance"},
	{"auth", "agent_error.provider_auth_or_access", "API Error: 401 invalid api key"},
	{"config", "agent_error.missing_config", "no LLM provider configured"},
	{"model", "agent_error.model_not_found_or_unavailable", "model not found"},
	{"unknown", "agent_error.unknown", "something unclassified broke"},
}

func countTasks(t *testing.T, f *delegatedFailureFixture) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_task_queue WHERE agent_id IN ($1, $2)`, f.coordinator, f.worker).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

// AC1 + AC2: the failure comment is on the timeline with literal type and text;
// nothing carries an agent mention and no task was enqueued.
func TestTerminalGatewayFailureKeepsCommentDropsAgentWake(t *testing.T) {
	for _, tc := range gatewayClassFailures {
		t.Run(tc.name, func(t *testing.T) {
			f, svc := seedDelegatedFailureFixture(t)
			ctx := context.Background()
			failedID := f.insertWorkerTask(t, "running", "comment", 3, 3)
			before := countTasks(t, f)

			if _, err := svc.FailTask(ctx, failedID, tc.errMsg, "", "", "", tc.reason, false, "", ""); err != nil {
				t.Fatalf("FailTask: %v", err)
			}

			// AC1: failure comment on the worker issue timeline.
			var typ, content string
			if err := f.pool.QueryRow(ctx, `
				SELECT type, content FROM comment
				WHERE issue_id = $1 AND author_type = 'agent' AND source_task_id = $2`, f.workerIssue, failedID).
				Scan(&typ, &content); err != nil {
				t.Fatalf("failure comment missing: %v", err)
			}
			if typ != "system" {
				t.Fatalf("failure comment type = %q, want %q", typ, "system")
			}
			if content != tc.errMsg {
				t.Fatalf("failure comment = %q, want %q", content, tc.errMsg)
			}

			// AC2: no agent mention anywhere on either issue, no new task.
			var mentions int
			if err := f.pool.QueryRow(ctx, `
				SELECT count(*) FROM comment
				WHERE issue_id IN ($1, $2) AND content LIKE '%mention://agent/%'`, f.issueID, f.workerIssue).
				Scan(&mentions); err != nil {
				t.Fatalf("count mentions: %v", err)
			}
			if mentions != 0 {
				t.Fatalf("agent mention comments = %d, want 0", mentions)
			}
			if after := countTasks(t, f); after != before {
				t.Fatalf("agent_task_queue rows = %d, want %d (no new task)", after, before)
			}
			var recovery, recoveryComments int
			if err := f.pool.QueryRow(ctx, `
				SELECT count(*) FROM agent_task_queue
				WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1`, failedID).
				Scan(&recovery); err != nil {
				t.Fatalf("count recovery tasks: %v", err)
			}
			if err := f.pool.QueryRow(ctx, `
				SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'progress_update' AND source_task_id = $2`,
				f.issueID, failedID).Scan(&recoveryComments); err != nil {
				t.Fatalf("count recovery comments: %v", err)
			}
			if recovery != 0 || recoveryComments != 0 {
				t.Fatalf("recovery tasks=%d comments=%d, want 0 each", recovery, recoveryComments)
			}
		})
	}
}

// AC3 (task row + task:failed event consumers): literal expectations.
func TestTerminalGatewayFailureKeepsTaskRowAndEvent(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	failedID := f.insertWorkerTask(t, "running", "comment", 3, 3)

	var mu sync.Mutex
	var got []events.Event
	svc.Bus.Subscribe(protocol.EventTaskFailed, func(e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e)
	})

	const errMsg = "API Error: Connection closed mid-response"
	if _, err := svc.FailTask(ctx, failedID, errMsg, "", "", "", "agent_error.provider_network", false, "", ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	var status, reason, storedErr string
	if err := f.pool.QueryRow(ctx, `SELECT status, failure_reason, error FROM agent_task_queue WHERE id = $1`, failedID).
		Scan(&status, &reason, &storedErr); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status != "failed" || reason != "agent_error.provider_network" || storedErr != errMsg {
		t.Fatalf("task = (%q, %q, %q), want (failed, agent_error.provider_network, %q)", status, reason, storedErr, errMsg)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("task:failed events = %d, want 1", len(got))
	}
	p, ok := got[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("task:failed payload type %T", got[0].Payload)
	}
	if p["status"] != "failed" || p["failure_reason"] != "agent_error.provider_network" ||
		p["retry_pending"] != false || p["error"] != errMsg {
		t.Fatalf("task:failed payload = %v", p)
	}
	if !strings.Contains(got[0].WorkspaceID, f.workspaceID) {
		t.Fatalf("task:failed workspace = %q, want %q", got[0].WorkspaceID, f.workspaceID)
	}
}

// AC2 guard: error text that itself carries an agent mention link must not
// reach the timeline as a live mention (a mention would enqueue a run).
func TestTerminalGatewayFailureNeutralizesMentionInErrorText(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	failedID := f.insertWorkerTask(t, "running", "comment", 3, 3)
	before := countTasks(t, f)

	errMsg := "upstream said: [@Bob](mention://agent/11111111-2222-3333-4444-555555555555) please retry"
	if _, err := svc.FailTask(ctx, failedID, errMsg, "", "", "", "agent_error.provider_network", false, "", ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	var content string
	if err := f.pool.QueryRow(ctx, `
		SELECT content FROM comment
		WHERE issue_id = $1 AND author_type = 'agent' AND source_task_id = $2`, f.workerIssue, failedID).
		Scan(&content); err != nil {
		t.Fatalf("failure comment missing: %v", err)
	}
	if strings.Contains(content, "mention://agent/") {
		t.Fatalf("failure comment keeps a live agent mention: %q", content)
	}
	if !strings.Contains(content, "upstream said:") {
		t.Fatalf("failure comment lost its text: %q", content)
	}
	if after := countTasks(t, f); after != before {
		t.Fatalf("agent_task_queue rows = %d, want %d (no new task)", after, before)
	}
}
