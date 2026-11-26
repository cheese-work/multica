package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-9926: strict priority order let a stream of newer higher-priority runs
// pass over an older lower-priority run without limit. A queued run gains one
// effective priority level per queueAgingStep, capped below urgent.
func TestQueuedRunAgesPastNewerHigherPriority(t *testing.T) {
	ctx := context.Background()
	runtime := func(f *claimDecisionFixture) string { return util.UUIDToString(f.runtimeID) }
	queue := func(f *claimDecisionFixture, agent, title string, prio int, age string) string {
		issue := f.fx.Issue(t, title)
		return f.fx.Task(t, agent, testutil.Cols{
			"runtime_id": runtime(f), "issue_id": issue, "status": "queued", "priority": prio,
			"created_at": testutil.Raw("now() - interval '" + age + "'"),
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

	t.Run("same agent: five-hour-old priority 0 beats a fresh priority 2", func(t *testing.T) {
		f := newClaimDecisionFixture(t, 1)
		agent := util.UUIDToString(f.agentID)
		old := queue(f, agent, "old low", 0, "5 hours")
		fresh := queue(f, agent, "fresh medium", 2, "1 minute")
		if got := claim(f); got != old {
			t.Fatalf("claimed %q, want the aged task %s ahead of fresh %s", got, old, fresh)
		}
	})

	t.Run("two agents on one runtime: the starved agent is tried first", func(t *testing.T) {
		f := newClaimDecisionFixture(t, 1)
		other := f.fx.Agent(t, "claim-aging-other", runtime(f), testutil.Cols{"max_concurrent_tasks": 1})
		old := queue(f, other, "old low", 0, "5 hours")
		fresh := queue(f, util.UUIDToString(f.agentID), "fresh high", 3, "1 minute")
		if got := claim(f); got != old {
			t.Fatalf("claimed %q, want the aged task %s ahead of fresh %s", got, old, fresh)
		}
	})

	t.Run("a young lower-priority run still waits", func(t *testing.T) {
		f := newClaimDecisionFixture(t, 1)
		agent := util.UUIDToString(f.agentID)
		queue(f, agent, "young low", 0, "1 minute")
		high := queue(f, agent, "fresh high", 3, "1 minute")
		if got := claim(f); got != high {
			t.Fatalf("claimed %q, want priority 3 task %s", got, high)
		}
	})

	t.Run("aging never outranks urgent", func(t *testing.T) {
		f := newClaimDecisionFixture(t, 1)
		agent := util.UUIDToString(f.agentID)
		queue(f, agent, "ancient low", 0, "48 hours")
		urgent := queue(f, agent, "fresh urgent", 4, "1 minute")
		if got := claim(f); got != urgent {
			t.Fatalf("claimed %q, want urgent task %s", got, urgent)
		}
	})
}
