package service

import "sync"

// runBudgetHooks counts run-budget hooks still running. A sync.WaitGroup does
// not fit: an enqueue may start a hook while a test waits, and a WaitGroup
// forbids an Add from zero concurrent with Wait.
var runBudgetHooks = struct {
	mu      sync.Mutex
	running int
	idle    *sync.Cond
}{}

func init() { runBudgetHooks.idle = sync.NewCond(&runBudgetHooks.mu) }

// goRunBudgetHook runs a run-budget hook on its own goroutine, so the hook
// writes the issue on its own connection after the enqueue transaction ends.
// The count is taken before the goroutine starts and never blocks the caller.
func goRunBudgetHook(fn func()) {
	runBudgetHooks.mu.Lock()
	runBudgetHooks.running++
	runBudgetHooks.mu.Unlock()
	go func() {
		defer func() {
			runBudgetHooks.mu.Lock()
			runBudgetHooks.running--
			if runBudgetHooks.running == 0 {
				runBudgetHooks.idle.Broadcast()
			}
			runBudgetHooks.mu.Unlock()
		}()
		fn()
	}()
}

// WaitRunBudgetHooks returns once every run-budget hook started so far has
// returned. Tests call it before they rewrite fields of a shared TaskService
// that a hook reads (CHE-4008).
func WaitRunBudgetHooks() {
	runBudgetHooks.mu.Lock()
	for runBudgetHooks.running > 0 {
		runBudgetHooks.idle.Wait()
	}
	runBudgetHooks.mu.Unlock()
}
