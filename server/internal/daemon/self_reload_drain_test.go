package daemon

import (
	"context"
	"testing"
	"time"
)

// CHE-5453: a pending reload must drain the daemon by itself. With tasks always
// running, "defer to the next tick" never finds an idle moment.
func TestTrySelfReload_DrainsThenReloadsWhenLastTaskEnds(t *testing.T) {
	d, restartCalls := newSelfReloadTestDaemon(t, "0.3.7")
	stubSelfVersion(t, "0.3.8", nil)
	d.reloadKick = make(chan struct{}, 1)
	t.Cleanup(func() { d.endReloadDrain(false) }) // stop the drain timer
	d.activeTasks.Store(2)

	d.trySelfReload(context.Background())

	if restartCalls.Load() != 0 {
		t.Fatal("reload fired with 2 tasks running")
	}
	if !d.isDraining() {
		t.Fatal("a pending reload with a busy daemon must start draining")
	}
	if d.tryEnterClaim() {
		t.Fatal("a draining daemon must claim nothing new")
	}

	d.taskDone() // 2 -> 1: still busy, no kick
	select {
	case <-d.reloadKick:
		t.Fatal("kicked while a task was still running")
	default:
	}

	d.taskDone() // 1 -> 0: kick
	select {
	case <-d.reloadKick:
	default:
		t.Fatal("last task ended while draining but the reload was not kicked")
	}
	d.trySelfReload(context.Background())
	if restartCalls.Load() != 1 {
		t.Fatalf("triggerRestart fired %d times after the daemon went idle, want 1", restartCalls.Load())
	}
}

func TestAutoUpdateLoop_ReloadsOnKickWithoutWaitingForTick(t *testing.T) {
	d, restartCalls := newSelfReloadTestDaemon(t, "0.3.7")
	stubSelfVersion(t, "0.3.8", nil)
	d.reloadKick = make(chan struct{}, 1)
	t.Cleanup(func() { d.endReloadDrain(false) }) // stop the drain timer
	d.activeTasks.Store(1)
	origDelay, origInterval := autoUpdateInitialDelay, selfReloadCheckInterval
	t.Cleanup(func() { autoUpdateInitialDelay, selfReloadCheckInterval = origDelay, origInterval })
	autoUpdateInitialDelay, selfReloadCheckInterval = time.Millisecond, time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.autoUpdateLoop(ctx)
	waitFor(t, d.isDraining, "loop never started draining")

	d.taskDone()
	waitFor(t, func() bool { return restartCalls.Load() == 1 }, "kick did not reload before the next tick")
}

func TestTrySelfReload_DrainTimeoutResumesClaimingThenRetries(t *testing.T) {
	d, _ := newSelfReloadTestDaemon(t, "0.3.7")
	stubSelfVersion(t, "0.3.8", nil)
	d.reloadKick = make(chan struct{}, 1)
	t.Cleanup(func() { d.endReloadDrain(false) }) // stop the drain timer
	d.activeTasks.Store(1)
	origTimeout := reloadDrainTimeout
	t.Cleanup(func() { reloadDrainTimeout = origTimeout })
	reloadDrainTimeout = 10 * time.Millisecond

	d.trySelfReload(context.Background())
	waitFor(t, func() bool { return !d.isDraining() }, "drain never timed out")
	if !d.tryEnterClaim() {
		t.Fatal("claiming must resume after the drain times out")
	}
	d.exitClaim()

	// Inside the retry backoff a tick does not re-drain...
	d.trySelfReload(context.Background())
	if d.isDraining() {
		t.Fatal("re-drained inside the retry backoff")
	}
	// ...the first tick after it does.
	d.claimMu.Lock()
	d.drainRetryAt = time.Now().Add(-time.Second)
	d.claimMu.Unlock()
	d.trySelfReload(context.Background())
	if !d.isDraining() {
		t.Fatal("did not retry the drain after the backoff")
	}
}

func TestTrySelfReload_VersionMatchEndsDrain(t *testing.T) {
	d, _ := newSelfReloadTestDaemon(t, "0.3.7")
	stubSelfVersion(t, "0.3.8", nil)
	d.reloadKick = make(chan struct{}, 1)
	t.Cleanup(func() { d.endReloadDrain(false) }) // stop the drain timer
	d.activeTasks.Store(1)
	d.trySelfReload(context.Background())
	if !d.isDraining() {
		t.Fatal("precondition: draining")
	}
	d.cfg.CLIVersion = "0.3.8" // operator reverted/caught up
	d.trySelfReload(context.Background())
	if d.isDraining() {
		t.Fatal("drain must end once the on-disk version matches")
	}
}
