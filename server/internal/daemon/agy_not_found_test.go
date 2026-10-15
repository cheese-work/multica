package daemon

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

const agyNotFound = "agent executor error: generating and executing: Error 404, Message: Requested entity was not found., Status: NOT_FOUND, Details: []"

func TestAGYNotFoundRetiresSession(t *testing.T) {
	reason, poisoned := classifyPoisonedError(agyNotFound)
	if !poisoned || reason != FailureReasonAPIInvalidRequest {
		t.Fatalf("classifyPoisonedError = (%q, %v), want (api_invalid_request, true)", reason, poisoned)
	}
}

// TestAGYNotFoundStaysFailedWhenFreshRetryWouldComplete is the CHE-1710
// current-404 control. A replayed 404 and a genuine current 404 carry the same
// text, so the in-turn fresh retry must not open: a completed retry would
// replace the 404 and report the task as a success. The turn fails once, and
// classifyPoisonedError retires the session for the next trigger.
func TestAGYNotFoundStaysFailedWhenFreshRetryWouldComplete(t *testing.T) {
	d := newTestDaemon(t)
	fb := &fakeBackend{
		results: []agent.Result{
			{Status: "failed", Error: agyNotFound, SessionID: "agy-session"},
			{Status: "completed", Output: "answer", SessionID: "agy-fresh"},
		},
	}

	opts := agent.ExecOptions{ResumeSessionID: "agy-session"}
	result, tools, err := d.executeAndDrain(context.Background(), fb, "p", opts, slog.Default(), "t", "", new(atomic.Int32))
	if err != nil {
		t.Fatal(err)
	}
	if shouldRetryWithFreshSession(result, opts.ResumeSessionID, tools, "antigravity") {
		t.Fatal("an AGY NOT_FOUND must not take the fresh-session retry: a completed retry would turn a genuine current 404 into success")
	}
	if result.Status != "failed" || result.Error != agyNotFound {
		t.Fatalf("result = (%q, %q), want the 404 reported as failed", result.Status, result.Error)
	}
	if n := fb.idx.Load(); n != 1 {
		t.Fatalf("backend calls = %d, want 1 (no fresh retry)", n)
	}
	if _, poisoned := classifyPoisonedError(result.Error); !poisoned {
		t.Fatal("the failed turn must still retire the session")
	}
}
