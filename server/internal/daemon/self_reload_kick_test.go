package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

// CHE-5881: a pending reload never pauses claims. It restarts at the first
// idle instant (kick) and otherwise waits, claims untouched.
func TestTrySelfReload_PendingReloadNeverStopsClaims(t *testing.T) {
	d, restartCalls := newSelfReloadTestDaemon(t, "0.3.7")
	stubSelfVersion(t, "0.3.8", nil)
	d.reloadKick = make(chan struct{}, 1)
	d.activeTasks.Store(2)

	// Fake clock: 45 simulated minutes in 1-minute steps, one reload tick each.
	// Nothing reads the wall clock, so the old 30 min drain timeout cannot
	// rescue a daemon that stops claiming on the first tick.
	for minute := 0; minute <= 45; minute++ {
		d.trySelfReload(context.Background())
		if d.reloadPending() == "" {
			t.Fatalf("minute %d: reload not pending", minute)
		}
		if !d.tryEnterClaim() {
			t.Fatalf("minute %d: claims stopped for a pending reload with 2 tasks running", minute)
		}
		d.exitClaim()
	}
	if restartCalls.Load() != 0 {
		t.Fatal("reload fired with 2 tasks running")
	}
}

func TestTaskDone_KicksPendingReloadOnLastTask(t *testing.T) {
	d, restartCalls := newSelfReloadTestDaemon(t, "0.3.7")
	stubSelfVersion(t, "0.3.8", nil)
	d.reloadKick = make(chan struct{}, 1)
	d.activeTasks.Store(2)
	d.trySelfReload(context.Background()) // busy: pending, no restart

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
		t.Fatal("last task ended with a reload pending but the reload was not kicked")
	}
	d.trySelfReload(context.Background())
	if restartCalls.Load() != 1 {
		t.Fatalf("triggerRestart fired %d times after the daemon went idle, want 1", restartCalls.Load())
	}
}

func TestTaskDone_NoKickWithoutPendingReload(t *testing.T) {
	d, _ := newSelfReloadTestDaemon(t, "0.3.7")
	d.reloadKick = make(chan struct{}, 1)
	d.activeTasks.Store(1)
	d.taskDone()
	select {
	case <-d.reloadKick:
		t.Fatal("kicked with no reload pending")
	default:
	}
}

func TestAutoUpdateLoop_ReloadsOnKickWithoutWaitingForTick(t *testing.T) {
	d, restartCalls := newSelfReloadTestDaemon(t, "0.3.7")
	stubSelfVersion(t, "0.3.8", nil)
	d.reloadKick = make(chan struct{}, 1)
	d.activeTasks.Store(1)
	origDelay, origInterval := autoUpdateInitialDelay, selfReloadCheckInterval
	t.Cleanup(func() { autoUpdateInitialDelay, selfReloadCheckInterval = origDelay, origInterval })
	autoUpdateInitialDelay, selfReloadCheckInterval = time.Millisecond, time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.autoUpdateLoop(ctx)
	waitFor(t, func() bool { return d.reloadPending() != "" }, "loop never marked the reload pending")

	d.taskDone()
	waitFor(t, func() bool { return restartCalls.Load() == 1 }, "kick did not reload before the next tick")
}

// A restart that cannot be scheduled must not leave the claim barrier set.
func TestTaskDone_KickedReloadReleasesBarrierWhenRestartCannotBeScheduled(t *testing.T) {
	d, restartCalls := newSelfReloadTestDaemon(t, "0.3.7")
	stubSelfVersion(t, "0.3.8", nil)
	d.reloadKick = make(chan struct{}, 1)
	d.activeTasks.Store(1)
	d.trySelfReload(context.Background())

	// The probe resolves the binary, then the handoff's resolve fails.
	probeResolve := resolveSelfExecutable
	calls := 0
	resolveSelfExecutable = func() (string, error) {
		calls++
		if calls == 1 {
			return probeResolve()
		}
		return "", errors.New("cannot resolve executable")
	}
	d.taskDone()
	<-d.reloadKick
	d.trySelfReload(context.Background())

	if restartCalls.Load() != 0 {
		t.Fatal("restart fired without a resolvable binary")
	}
	if claimsPaused(t, d) {
		t.Fatal("claim barrier left set after the restart could not be scheduled")
	}
}

// A stalled update lookup (tryAutoUpdate, network) must not hold the kick: the
// reload runs independently of the pull half, so the last task ending restarts
// the daemon within 1 s even while the lookup is still blocked.
func TestAutoUpdateLoop_KickReloadsWhileUpdateLookupIsStalled(t *testing.T) {
	d, restartCalls := newSelfReloadTestDaemon(t, "0.3.7")
	d.cfg.AutoUpdateEnabled = true
	stubSelfVersion(t, "0.3.8", nil)
	stubFork(t, nil, nil)
	stalled := make(chan struct{})
	resolveForkUpdate = func(ctx context.Context, _, _, _, _, _ string) (*cli.ForkUpdate, error) {
		close(stalled)
		<-ctx.Done() // the lookup never answers
		return nil, ctx.Err()
	}
	d.reloadKick = make(chan struct{}, 1)
	d.activeTasks.Store(1)
	origDelay, origInterval := autoUpdateInitialDelay, selfReloadCheckInterval
	t.Cleanup(func() { autoUpdateInitialDelay, selfReloadCheckInterval = origDelay, origInterval })
	autoUpdateInitialDelay, selfReloadCheckInterval = time.Millisecond, time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.autoUpdateLoop(ctx)
	<-stalled
	waitFor(t, func() bool { return d.reloadPending() != "" }, "reload never became pending while the lookup stalled")

	d.taskDone()
	start := time.Now()
	waitFor(t, func() bool { return restartCalls.Load() == 1 }, "kick did not reload while the update lookup was stalled")
	if took := time.Since(start); took > time.Second {
		t.Fatalf("restart took %v after the last task ended, want under 1s", took)
	}
}
