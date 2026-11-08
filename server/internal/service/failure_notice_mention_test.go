package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-4579 (CHE-2804 item 2): a terminal failure stays on the issue timeline,
// but the notice must not mention an agent that cannot act. The daemon error
// text is untrusted and may carry agent mention markup.
func TestFailureNoticeAgentMention(t *testing.T) {
	const (
		rawError = "stream died, ask [@Other](mention://agent/11111111-2222-3333-4444-555555555555) to retry"
		plain    = "stream died, ask @Other to retry"
	)
	cases := []struct {
		name  string
		setup string // SQL run on the failing agent before the failure
		want  string
	}{
		{"agent can act keeps the text as is", "", rawError},
		{"archived agent loses the mention", "UPDATE agent SET archived_at = now() WHERE id = $1", plain},
		{"agent without a runtime loses the mention", "UPDATE agent SET runtime_id = NULL WHERE id = $1", plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newOwnerFixture(t)
			ctx := context.Background()
			// A recovery-lineage task is capped, so FailTask also sends the owner inbox notice.
			task := o.recoveryTask(t, nil)
			if tc.setup != "" {
				o.Exec(t, tc.setup, o.agentID)
			}
			var failedEvents int
			o.svc.TaskSvc.Bus.Subscribe(protocol.EventTaskFailed, func(events.Event) { failedEvents++ })

			if _, err := o.svc.TaskSvc.FailTask(ctx, parseTestUUID(t, task), rawError, "", "", "", "timeout", false, "", ""); err != nil {
				t.Fatal(err)
			}

			var got string
			if err := o.Pool.QueryRow(ctx, `SELECT content FROM comment WHERE issue_id=$1 AND type='system'`, o.issueID).Scan(&got); err != nil {
				t.Fatalf("failure comment missing from the issue: %v", err)
			}
			if got != tc.want {
				t.Fatalf("failure comment = %q, want %q", got, tc.want)
			}
			if failedEvents != 1 {
				t.Fatalf("task failed events = %d, want 1", failedEvents)
			}
			if n := o.countNotices(t, task); n != 1 {
				t.Fatalf("owner inbox notices = %d, want 1", n)
			}
			var queued int
			if err := o.Pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status IN ('queued','dispatched')`, o.issueID).Scan(&queued); err != nil || queued != 0 {
				t.Fatalf("runs enqueued by the failure = %d err=%v, want none", queued, err)
			}
		})
	}
}
