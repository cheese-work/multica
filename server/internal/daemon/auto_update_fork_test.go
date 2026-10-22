package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

const forkTestCommit = "901cfa043c"

// stubFork switches the daemon to the fork source with a fixed resolver result.
func stubFork(t *testing.T, u *cli.ForkUpdate, resolveErr error) *int {
	t.Helper()
	prevSrc, prevRes, prevCommit, prevApply := forkUpdateSource, resolveForkUpdate, currentCommit, applyForkUpdate
	applied := new(int)
	forkUpdateSource = func() bool { return true }
	currentCommit = func() string { return forkTestCommit }
	resolveForkUpdate = func(_ context.Context, _, _, _, _, cur string) (*cli.ForkUpdate, error) {
		if cur != forkTestCommit {
			t.Errorf("resolver got current commit %q", cur)
		}
		return u, resolveErr
	}
	applyForkUpdate = func(context.Context, *cli.ForkUpdate, time.Duration) (string, error) {
		*applied++
		return "installed", nil
	}
	t.Cleanup(func() {
		forkUpdateSource, resolveForkUpdate, currentCommit, applyForkUpdate = prevSrc, prevRes, prevCommit, prevApply
	})
	return applied
}

var forkCandidate = &cli.ForkUpdate{HeadSHA: strings.Repeat("a", 40), Name: "linux-amd64", Digest: strings.Repeat("0", 64)}

func TestTryAutoUpdate_ForkSourceUpdatesToArtifactCommit(t *testing.T) {
	d, restartCalls := newAutoUpdateTestDaemon(t, "v0.1.13-235-gabcdef0") // dev-describe version: fork source must not care
	stubFork(t, forkCandidate, nil)
	var target string
	d.runUpdateFn = func(tv string) (string, error) { target = tv; return "ok", nil }

	d.tryAutoUpdate(context.Background())

	if target != forkCandidate.HeadSHA || restartCalls.Load() != 1 {
		t.Fatalf("target=%q restarts=%d", target, restartCalls.Load())
	}
}

func TestTryAutoUpdate_ForkSourceInstallsWhileBusyAndReloadsWhenIdle(t *testing.T) {
	if !cli.ForkUpdateSupported(runtime.GOOS) {
		t.Skip("fork source is Linux/macOS only")
	}
	for _, busy := range []string{"task running", "claim in flight", "task starts during install"} {
		t.Run(busy, func(t *testing.T) {
			testDaemon, restartCalls := newSelfReloadTestDaemon(t, "v0.1.13")
			applied := stubFork(t, forkCandidate, nil)
			stubSelfVersion(t, "v0.1.14", nil)
			switch busy {
			case "task running":
				testDaemon.activeTasks.Store(1)
			case "claim in flight":
				testDaemon.claimsInFlight = 1
			}
			testDaemon.runUpdateFn = func(target string) (string, error) {
				if target != forkCandidate.HeadSHA {
					t.Fatalf("update target = %q, want %q", target, forkCandidate.HeadSHA)
				}
				if testDaemon.pauseClaims || !testDaemon.updating.Load() {
					t.Fatalf("install must hold update ownership without pausing claims: pause=%v updating=%v", testDaemon.pauseClaims, testDaemon.updating.Load())
				}
				if busy == "task starts during install" {
					testDaemon.activeTasks.Store(1)
				}
				return testDaemon.runUpdate(target)
			}

			testDaemon.tryAutoUpdate(context.Background())

			if *applied != 1 {
				t.Fatalf("fork installs = %d, want 1 while %s", *applied, busy)
			}
			if restartCalls.Load() != 0 || testDaemon.RestartBinary() != "" || testDaemon.pauseClaims || testDaemon.updating.Load() {
				t.Fatalf("restart must defer without holding claims or update ownership: restarts=%d binary=%q pause=%v updating=%v", restartCalls.Load(), testDaemon.RestartBinary(), testDaemon.pauseClaims, testDaemon.updating.Load())
			}

			testDaemon.trySelfReload(context.Background())
			if restartCalls.Load() != 0 || testDaemon.pauseClaims || testDaemon.updating.Load() {
				t.Fatalf("auto-reload must also defer while busy: restarts=%d pause=%v updating=%v", restartCalls.Load(), testDaemon.pauseClaims, testDaemon.updating.Load())
			}

			testDaemon.activeTasks.Store(0)
			testDaemon.claimsInFlight = 0
			testDaemon.trySelfReload(context.Background())
			if restartCalls.Load() != 1 || testDaemon.RestartBinary() == "" || !testDaemon.pauseClaims || !testDaemon.updating.Load() {
				t.Fatalf("idle auto-reload must restart with the barrier held: restarts=%d binary=%q pause=%v updating=%v", restartCalls.Load(), testDaemon.RestartBinary(), testDaemon.pauseClaims, testDaemon.updating.Load())
			}
			if *applied != 1 {
				t.Fatalf("auto-reload reinstalled the binary: installs=%d", *applied)
			}
		})
	}
}

func TestTryAutoUpdate_ForkSourceKeepsDaemonWithoutEligibleBuild(t *testing.T) {
	for name, err := range map[string]error{
		"up to date":         cli.ErrForkUpToDate,
		"no platform build":  fmt.Errorf("%w: server has no darwin-arm64 build", cli.ErrForkNoArtifact),
		"integrity or other": errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) {
			d, restartCalls := newAutoUpdateTestDaemon(t, "v0.1.13")
			stubFork(t, nil, err)
			withStubRelease(t, &cli.GitHubRelease{TagName: "v9.9.9"}, nil) // upstream must never be consulted
			forkUpdateSource = func() bool { return true }

			d.tryAutoUpdate(context.Background())

			if restartCalls.Load() != 0 || d.pauseClaims || d.updating.Load() {
				t.Fatalf("daemon state changed: restarts=%d pause=%v updating=%v", restartCalls.Load(), d.pauseClaims, d.updating.Load())
			}
		})
	}
}

func TestAutoUpdateLoop_ForkSourceSkipsWithoutBuildCommit(t *testing.T) {
	d := &Daemon{cfg: Config{AutoUpdateEnabled: true, CLIVersion: "v0.1.13"}, logger: slog.Default()}
	d.runUpdateFn = func(string) (string, error) { t.Fatal("runUpdateFn called"); return "", nil }
	stubFork(t, nil, nil)
	currentCommit = func() string { return "unknown" }

	done := make(chan struct{})
	go func() { d.autoUpdateLoop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("autoUpdateLoop did not exit for an unknown build commit")
	}
}

// runUpdate (shared by the server-triggered and poller paths) must use the fork
// source on Linux/macOS and never reach Homebrew or upstream downloads.
func TestRunUpdate_ForkSourceFailureReportsAndKeepsInstalled(t *testing.T) {
	if !cli.ForkUpdateSupported(runtime.GOOS) {
		t.Skip("fork source is Linux/macOS only")
	}
	d, _ := newAutoUpdateTestDaemon(t, "v0.1.13")
	d.runUpdateFn = d.runUpdate
	applied := stubFork(t, nil, fmt.Errorf("%w: server has no darwin-arm64 build", cli.ErrForkNoArtifact))
	forkUpdateSource = func() bool { return true }

	out, err := d.runUpdate("v9.9.9")

	if err == nil || !errors.Is(err, cli.ErrForkNoArtifact) || !strings.Contains(err.Error(), "installed daemon unchanged") {
		t.Fatalf("err = %v", err)
	}
	if *applied != 0 || out != "" {
		t.Fatalf("applied=%d out=%q", *applied, out)
	}
}

func TestRunUpdate_ForkSourceAppliesResolvedCandidate(t *testing.T) {
	if !cli.ForkUpdateSupported(runtime.GOOS) {
		t.Skip("fork source is Linux/macOS only")
	}
	d, _ := newAutoUpdateTestDaemon(t, "v0.1.13")
	applied := stubFork(t, forkCandidate, nil)

	out, err := d.runUpdate("v9.9.9")

	if err != nil || out != "installed" || *applied != 1 {
		t.Fatalf("out=%q err=%v applied=%d", out, err, *applied)
	}
}
