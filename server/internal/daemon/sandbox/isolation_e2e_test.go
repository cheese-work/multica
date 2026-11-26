//go:build sandbox_e2e && linux

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireE2ESandbox skips with a printed reason when the host cannot run the
// sandbox. Under CI=true with bwrap installed a skip would hide a broken lane,
// so it is a failure there.
func requireE2ESandbox(t *testing.T, l *Launcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := l.SelfCheck(ctx)
	if err == nil {
		return
	}
	_, lookErr := exec.LookPath(l.bwrap())
	if skipIsFailure(os.Getenv("CI"), lookErr == nil) {
		t.Fatalf("CI=true and bwrap is present, but the sandbox self-check failed (a skip is a failure here): %v", err)
	}
	t.Skipf("sandbox_e2e: host cannot run the sandbox: %v", err)
}

// skipIsFailure is the skip rule: with CI=true and bwrap present, a host that
// cannot run the sandbox is a broken lane, not a reason to skip.
func skipIsFailure(ci string, bwrapPresent bool) bool { return ci == "true" && bwrapPresent }

func TestSkipIsFailureRule(t *testing.T) {
	for _, c := range []struct {
		ci      string
		bwrap   bool
		failure bool
	}{{"true", true, true}, {"true", false, false}, {"", true, false}, {"false", true, false}} {
		if got := skipIsFailure(c.ci, c.bwrap); got != c.failure {
			t.Errorf("CI=%q bwrap=%v: failure=%v, want %v", c.ci, c.bwrap, got, c.failure)
		}
	}
}

// startRun launches script in its own sandbox with env and returns once the
// script wrote its ready file. The run is killed when the test ends.
func startRun(t *testing.T, l *Launcher, name, script string, env []string) {
	t.Helper()
	ly := newLayout(t)
	ready := filepath.Join(ly.work, name+".ready")
	agent := fakeAgent(t, ly.work, "touch "+ready+"\n"+script)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, agent)
	cmd.WaitDelay = 3 * time.Second
	cmd.Env = env
	run, err := l.Wrap(cmd, Spec{WorkDir: ly.work, TempDir: ly.tmp, Home: ly.home, NoDaemonAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	run.Started()
	t.Cleanup(func() { cancel(); _ = cmd.Wait(); run.Close() })
	for range 200 {
		if _, err := os.Stat(ready); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("run %s never became ready", name)
}

// findMarker runs script in a sandbox and returns its output.
func findMarker(t *testing.T, l *Launcher, script string) string {
	t.Helper()
	ly := newLayout(t)
	agent := fakeAgent(t, ly.work, script)
	cmd := exec.Command(agent)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	run, err := l.Wrap(cmd, Spec{WorkDir: ly.work, TempDir: ly.tmp, Home: ly.home, NoDaemonAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe run: %v\n%s", err, out)
	}
	return string(out)
}

// X3 (CHE-10369, plan RSC-1 R4): under --unshare-pid, run B sees no process of
// run A. A process is found by its environ (marker variable) and by its command
// line (a unique sleep argument): the kernel's ptrace rule can hide environ
// across user namespaces on its own, so the command line is the check that only
// the pid namespace satisfies. A control run proves the probe finds both when
// they are visible. Build tag sandbox_e2e, a Linux host with bubblewrap.
func TestSandboxHidesOtherRunEnviron(t *testing.T) {
	l := newLauncher()
	requireE2ESandbox(t, l)
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	marker := "RUN_A_MARKER_" + hex.EncodeToString(raw)
	arg := "4000." + strconv.FormatUint(binary.BigEndian.Uint64(raw)%900000000+100000000, 10) // a sleep time no other process uses

	startRun(t, l, "a", "sleep "+arg+" &\nwait\n", []string{"PATH=/usr/bin:/bin", marker + "=1"})

	// [s]leep: the grep's own command line must not match its pattern.
	probe := "for f in /proc/[0-9]*/environ; do tr '\\0' '\\n' < \"$f\" 2>/dev/null | grep -q '^" + marker + "=' && echo \"FOUND environ $f\"; done\n" +
		"for f in /proc/[0-9]*/cmdline; do tr '\\0' ' ' < \"$f\" 2>/dev/null | grep -q '[s]leep " + arg + "' && echo \"FOUND cmdline $f\"; done\n" +
		"echo probe-done\n"
	got := findMarker(t, l, probe)
	if !strings.Contains(got, "probe-done") {
		t.Fatalf("probe did not finish: %q", got)
	}
	if strings.Contains(got, "FOUND") {
		t.Fatalf("run B can see run A's process: %q", got)
	}

	// Control: the same marker and command line inside B's own sandbox are found.
	own := findMarker(t, l, "export "+marker+"=1\nsleep "+arg+" &\n"+probe)
	if !strings.Contains(own, "FOUND environ") || !strings.Contains(own, "FOUND cmdline") {
		t.Fatalf("probe cannot see a process in its own sandbox, so the check above proves nothing: %q", own)
	}
}
