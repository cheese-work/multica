package handler

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const progressBinarySize = 5 * 512 * 1024 // 2.5 MiB, five fake-curl chunks

// bigBinary is a runnable fake CLI padded with comment lines to size bytes.
func bigBinary(size int) string {
	b := fakeBinary(installTestCommit[:7])
	var sb strings.Builder // one allocation; repeated string += was quadratic at 2.5 MiB
	sb.Grow(size)
	sb.WriteString(b)
	pad := strings.Repeat("#", 99) + "\n"
	for sb.Len()+len(pad) <= size {
		sb.WriteString(pad)
	}
	if r := size - sb.Len(); r > 0 {
		sb.WriteString(strings.Repeat("#", r-1) + "\n")
	}
	return sb.String()
}

type ttyRun struct {
	out      string
	err      error
	took     time.Duration
	bgPIDLog string
}

// runInstallTTY runs the served script under script(1), so stderr is a TTY.
// The curl wrapper hands the binary over in slow chunks (failAfter > 0 makes it
// die with curl's exit 18 after that many chunks) and logs its own pid.
func runInstallTTY(t *testing.T, f *installFake, failAfter int, killAfter time.Duration) ttyRun {
	t.Helper()
	scriptPath, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) not installed")
	}
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not installed")
	}
	resp, err := http.Get(f.srv.URL + "/api/daemon/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	tmp := t.TempDir()
	wrap := filepath.Join(tmp, "wrap")
	_ = os.MkdirAll(wrap, 0o755)
	instDir := filepath.Join(tmp, "bin")
	_ = os.MkdirAll(instDir, 0o755)
	installSh := filepath.Join(tmp, "install.sh")
	_ = os.WriteFile(installSh, body, 0o644)
	pidLog := filepath.Join(tmp, "curl.pid")
	shellPidLog := filepath.Join(tmp, "shell.pid")

	// Only the binary download is slowed; the manifest goes straight to curl.
	wrapper := fmt.Sprintf(`#!/bin/sh
case "$*" in *update/binary*) ;; *) exec %[1]q "$@" ;; esac
trap "" HUP # closing the pty must not hide a leaked curl
echo $$ >> %[2]q
out=; args=
for a in "$@"; do
  if [ "$prev" = -o ]; then out=$a; a=$out.real; fi
  prev=$a; args="$args '$a'"
done
code=$(eval %[1]q $args) || exit $?
n=0
while [ $((n * 524288)) -lt $(wc -c <"$out.real") ]; do
  dd if="$out.real" bs=524288 skip=$n count=1 2>/dev/null >>"$out"
  n=$((n + 1))
  if [ %[3]d -gt 0 ] && [ $n -ge %[3]d ]; then exit 18; fi
  sleep 0.4
done
printf %%s "$code"
`, realCurl, pidLog, failAfter)
	if err := os.WriteFile(filepath.Join(wrap, "curl"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}

	inner := fmt.Sprintf("echo $$ > %q; exec bash %q", shellPidLog, installSh)
	cmd := exec.Command(scriptPath, "-qec", inner, "/dev/null")
	cmd.Env = []string{
		"PATH=" + wrap + ":" + os.Getenv("PATH"),
		"HOME=" + tmp,
		"MULTICA_INSTALL_DIR=" + instDir,
		"MULTICA_TOKEN=" + installTestToken,
		"TERM=xterm",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if killAfter > 0 {
		time.Sleep(killAfter)
		raw, _ := os.ReadFile(shellPidLog)
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	runErr := cmd.Wait()
	pidRaw, _ := os.ReadFile(pidLog)
	return ttyRun{out: out.String(), err: runErr, took: time.Since(start), bgPIDLog: string(pidRaw)}
}

func newProgressFake(t *testing.T, digest string) *installFake {
	return newInstallFake(t, bigBinary(progressBinarySize), digest)
}

func TestInstallScriptTTYRendersKnownSizeBar(t *testing.T) {
	r := runInstallTTY(t, newProgressFake(t, ""), 0, 0)
	if r.err != nil {
		t.Fatalf("install failed: %v\n%q", r.err, r.out)
	}
	for _, want := range []string{"100%", "2.5 / 2.5 MB", "Verifying sha256", "Probing --version", "Installing", "Installed"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("TTY output lacks %q:\n%q", want, r.out)
		}
	}
	// One erase-line marker per redraw: at most 5 per second, plus the final one.
	draws := strings.Count(r.out, "\x1b[K")
	if limit := int(r.took.Seconds()*5) + 2; draws < 2 || draws > limit {
		t.Errorf("redraws = %d, want 2..%d in %s", draws, limit, r.took)
	}
}

func TestInstallScriptTTYEndsLineBeforeFailure(t *testing.T) {
	r := runInstallTTY(t, newProgressFake(t, ""), 2, 0)
	if r.err == nil {
		t.Fatalf("a dropped download must fail:\n%q", r.out)
	}
	i := strings.Index(r.out, "✗ cannot reach")
	if i < 1 || r.out[i-1] != '\n' {
		t.Fatalf("failure message does not start its own line:\n%q", r.out)
	}
	wantExit1(t, r.err)
}

func TestInstallScriptTTYDigestMismatchKeepsMessageAndExit(t *testing.T) {
	r := runInstallTTY(t, newProgressFake(t, strings.Repeat("0", 64)), 0, 0)
	wantExit1(t, r.err)
	i := strings.Index(r.out, "✗ sha256 mismatch")
	if i < 1 || r.out[i-1] != '\n' {
		t.Fatalf("mismatch message does not start its own line:\n%q", r.out)
	}
}

func TestInstallScriptInterruptLeavesNoBackgroundCurl(t *testing.T) {
	r := runInstallTTY(t, newProgressFake(t, ""), 0, 900*time.Millisecond)
	if r.err == nil {
		t.Fatalf("a killed install must not succeed:\n%q", r.out)
	}
	pids := strings.Fields(r.bgPIDLog)
	if len(pids) == 0 {
		t.Fatalf("fake curl never started:\n%q", r.out)
	}
	time.Sleep(300 * time.Millisecond)
	for _, p := range pids {
		pid, _ := strconv.Atoi(p)
		if syscall.Kill(pid, 0) == nil {
			t.Errorf("background curl %d outlived the installer", pid)
		}
	}
}

// Without a TTY the output is exactly what the script printed before the renderer existed.
func TestInstallScriptNonTTYOutputUnchanged(t *testing.T) {
	f := newProgressFake(t, "")
	r := runInstall(t, f, installTestToken, "")
	if r.err != nil {
		t.Fatalf("install failed: %v\n%s", r.err, r.out)
	}
	want := fmt.Sprintf("==> Reading the server's build\n==> Downloading %s build (commit 0123456)\n==> Installed %s (commit 0123456)\n"+
		"Add %s to your PATH to run `multica`.\n\nNext:\n  multica config set server_url %s\n  multica login --token\n  multica daemon start\n",
		runtime.GOOS+"-"+runtime.GOARCH, r.bin, filepath.Dir(r.bin), f.srv.URL)
	if r.out != want {
		t.Fatalf("non-TTY output changed:\n got %q\nwant %q", r.out, want)
	}
}

func wantExit1(t *testing.T, err error) {
	t.Helper()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("exit = %v, want 1", err)
	}
}
