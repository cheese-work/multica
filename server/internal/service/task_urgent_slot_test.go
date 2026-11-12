package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-5118: an agent at max_concurrent_tasks may start ONE extra task, and only
// an urgent one. runtime_config.urgent_reserved_slot=false turns it off.
func TestUrgentTaskUsesReservedSlot(t *testing.T) {
	ctx := context.Background()
	newTask := func(f *claimDecisionFixture, title, status string, prio int) string {
		issue := f.fx.Issue(t, title)
		return f.fx.Task(t, util.UUIDToString(f.agentID), testutil.Cols{
			"runtime_id": util.UUIDToString(f.runtimeID), "issue_id": issue, "status": status, "priority": prio,
		})
	}
	claim := func(f *claimDecisionFixture) string {
		t.Helper()
		got, err := f.svc.ClaimTaskForRuntime(ctx, f.runtimeID)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			return ""
		}
		return util.UUIDToString(got.ID)
	}

	t.Run("urgent starts past the cap, once", func(t *testing.T) {
		f := newClaimDecisionFixture(t, 1)
		newTask(f, "busy", "running", 2)
		medium := newTask(f, "medium", "queued", 2)
		urgent := newTask(f, "urgent", "queued", 4)
		urgent2 := newTask(f, "urgent 2", "queued", 4)
		if got := claim(f); got != urgent {
			t.Fatalf("claimed %q, want the older urgent %s (not medium %s)", got, urgent, medium)
		}
		if got := claim(f); got != "" {
			t.Fatalf("claimed %q past the single reserved slot (urgent2 %s)", got, urgent2)
		}
	})
	t.Run("medium never uses it", func(t *testing.T) {
		f := newClaimDecisionFixture(t, 1)
		newTask(f, "busy", "running", 2)
		newTask(f, "medium", "queued", 2)
		if got := claim(f); got != "" {
			t.Fatalf("claimed %q at the cap, want none", got)
		}
	})
	t.Run("setting off", func(t *testing.T) {
		f := newClaimDecisionFixture(t, 1)
		f.fx.Exec(t, `UPDATE agent SET runtime_config = '{"urgent_reserved_slot": false}' WHERE id = $1`, util.UUIDToString(f.agentID))
		newTask(f, "busy", "running", 2)
		newTask(f, "urgent", "queued", 4)
		if got := claim(f); got != "" {
			t.Fatalf("claimed %q with the reserved slot disabled", got)
		}
	})
}
