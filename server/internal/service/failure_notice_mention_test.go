package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-4579 (CHE-2804 item 2): a terminal failure stays on the issue timeline,
// but the notice must not mention an agent that cannot act. The daemon error
// text is untrusted and may carry agent mention markup.
func TestFailureNoticeAgentMention(t *testing.T) {
	const (
		rawError = "stream died, ask [@Other](mention://agent/11111111-2222-3333-4444-555555555555) to retry"
		plain    = "stream died, ask @Other to retry"
		archived = "UPDATE agent SET archived_at = now() WHERE id = $1"
		// Labels may hold brackets, so stripping one link can assemble another.
		nestedAgent = "[@[@A](mention://agent/11111111-2222-3333-4444-555555555555)](mention://agent/66666666-7777-8888-9999-000000000000)"
		nestedAll   = "[@[@all](mention://agent/11111111-2222-3333-4444-555555555555)](mention://all/all)"
		nestedOpen  = "[@[@all](mention://all/all)](mention://agent/11111111-2222-3333-4444-555555555555)"
		issueLink   = "see [CHE-1](mention://issue/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee) and [@Other](mention://agent/11111111-2222-3333-4444-555555555555)"
	)
	cases := []struct {
		name  string
		setup string // SQL run on the failing agent before the failure
		raw   string // daemon error text
		want  string
	}{
		{"agent can act keeps the text as is", "", rawError, rawError},
		{"agent can act keeps nested links as is", "", nestedAll, nestedAll},
		{"archived agent loses the mention", archived, rawError, plain},
		{"agent without a runtime loses the mention", "UPDATE agent SET runtime_id = NULL WHERE id = $1", rawError, plain},
		{"standalone issue link stays live when the agent cannot act", archived, issueLink, "see [CHE-1](mention://issue/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee) and @Other"},
		{"nested agent labels do not rebuild a mention", archived, nestedAgent, "@[@A](mention-removed://agent/66666666-7777-8888-9999-000000000000)"},
		{"nested label does not rebuild an @all mention", archived, nestedAll, "@[@all](mention-removed://all/all)"},
		{"nested @all label inside an agent link is made inert", archived, nestedOpen, "@[@all](mention-removed://agent/11111111-2222-3333-4444-555555555555)"},
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

			if _, err := o.svc.TaskSvc.FailTask(ctx, parseTestUUID(t, task), tc.raw, "", "", "", "timeout", false, "", ""); err != nil {
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
			if tc.setup != "" {
				for _, m := range util.ParseMentions(got) {
					if m.Type == "issue" {
						continue
					}
					t.Fatalf("failure comment %q still mentions %s/%s", got, m.Type, m.ID)
				}
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

// CHE-4579 security review: the daemon error text is attacker-sized. Deep
// bracket nesting must neither leave a live mention nor cost more than linear work.
func TestFailureNoticeDeepNestingStaysBounded(t *testing.T) {
	const depth = 4000
	raw := strings.Repeat("[@", depth) + "x" + strings.Repeat("](mention://agent/11111111-2222-3333-4444-555555555555)", depth)
	o := newOwnerFixture(t)
	ctx := context.Background()
	task := o.recoveryTask(t, nil)
	o.Exec(t, "UPDATE agent SET archived_at = now() WHERE id = $1", o.agentID)

	start := time.Now()
	if _, err := o.svc.TaskSvc.FailTask(ctx, parseTestUUID(t, task), raw, "", "", "", "timeout", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("FailTask on %d nested links took %s, want well under 2s", depth, took)
	}
	var got string
	if err := o.Pool.QueryRow(ctx, `SELECT content FROM comment WHERE issue_id=$1 AND type='system'`, o.issueID).Scan(&got); err != nil {
		t.Fatalf("failure comment missing from the issue: %v", err)
	}
	if ms := util.ParseMentions(got); len(ms) != 0 {
		t.Fatalf("failure comment still mentions %s/%s", ms[0].Type, ms[0].ID)
	}
}
