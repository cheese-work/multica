package service

import (
	"testing"
	"time"
)

// A hook outlives the enqueue that started it. Tests that rewrite shared
// service fields wait for it first, or the race detector fails the package
// (CHE-4008: TxScoped read FeatureFlags while a test cleanup restored it).
func TestWaitRunBudgetHooksWaitsForStartedHooks(t *testing.T) {
	release := make(chan struct{})
	finished := false
	goRunBudgetHook(func() {
		<-release
		finished = true
	})

	waited := make(chan struct{})
	go func() {
		WaitRunBudgetHooks()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("WaitRunBudgetHooks returned while a started hook was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitRunBudgetHooks did not return after the hook finished")
	}
	if !finished {
		t.Fatal("WaitRunBudgetHooks returned before the hook's writes were visible")
	}
}
