package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	installTestToken  = "mul_supersecrettoken0123456789abcdef"
	installTestCommit = "0123456789abcdef0123456789abcdef01234567"
)

type installFake struct {
	srv       *httptest.Server
	binaryHit int
}

// newInstallFake serves the real install script publicly and fake
// authenticated update endpoints. digest, when set, replaces the manifest sha256.
func newInstallFake(t *testing.T, binary, digest string) *installFake {
	t.Helper()
	sum := sha256.Sum256([]byte(binary))
	if digest == "" {
		digest = hex.EncodeToString(sum[:])
	}
	key := runtime.GOOS + "-" + runtime.GOARCH
	f := &installFake{}
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+installTestToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/install.sh", DaemonInstallScript)
	mux.HandleFunc("/api/daemon/update", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"commit":%q,"version":"v9.9.9","binaries":{%q:{"sha256":%q,"size":%d}}}`,
			installTestCommit, key, digest, len(binary))
	}))
	mux.HandleFunc("/api/daemon/update/binary", auth(func(w http.ResponseWriter, r *http.Request) {
		f.binaryHit++
		if r.URL.Query().Get("os") != runtime.GOOS || r.URL.Query().Get("arch") != runtime.GOARCH {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, binary)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func fakeBinary(commit string) string {
	return fmt.Sprintf("#!/bin/sh\necho 'multica v9.9.9 (commit: %s, built: 2026-01-01)'\n", commit)
}

type installRun struct {
	out, argv, bin string
	err            error
}

// runInstall pipes the served script into bash (as `curl | bash` does), with
// no controlling terminal and a curl wrapper that records every argv.
func runInstall(t *testing.T, f *installFake, token string, preinstalled string, extraEnv ...string) installRun {
	t.Helper()
	resp, err := http.Get(f.srv.URL + "/api/daemon/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("install.sh: HTTP %d", resp.StatusCode)
	}

	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not installed")
	}
	tmp := t.TempDir()
	wrap := filepath.Join(tmp, "wrap")
	_ = os.MkdirAll(wrap, 0o755)
	argvLog := filepath.Join(tmp, "curl-argv.log")
	wrapper := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >> %q; done\nexec %q \"$@\"\n", argvLog, realCurl)
	if err := os.WriteFile(filepath.Join(wrap, "curl"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	instDir := filepath.Join(tmp, "bin")
	_ = os.MkdirAll(instDir, 0o755)
	binPath := filepath.Join(instDir, "multica")
	if preinstalled != "" {
		_ = os.WriteFile(binPath, []byte(preinstalled), 0o755)
	}

	cmd := exec.Command("bash")
	cmd.Stdin = strings.NewReader(string(script))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // no /dev/tty
	cmd.Env = []string{
		"PATH=" + wrap + ":" + os.Getenv("PATH"),
		"HOME=" + tmp,
		"MULTICA_INSTALL_DIR=" + instDir,
	}
	if token != "" {
		cmd.Env = append(cmd.Env, "MULTICA_TOKEN="+token)
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	out, runErr := cmd.CombinedOutput()
	argv, _ := os.ReadFile(argvLog)
	return installRun{out: string(out), argv: string(argv), bin: binPath, err: runErr}
}

func TestInstallScriptHappyPath(t *testing.T) {
	f := newInstallFake(t, fakeBinary(installTestCommit[:7]), "")
	r := runInstall(t, f, installTestToken, "")
	if r.err != nil {
		t.Fatalf("install failed: %v\n%s", r.err, r.out)
	}
	got, err := exec.Command(r.bin, "--version").Output()
	if err != nil || !strings.Contains(string(got), "commit: "+installTestCommit[:7]) {
		t.Fatalf("installed binary not runnable/expected: %v %q", err, got)
	}
	if f.binaryHit != 1 {
		t.Fatalf("binary endpoint hits = %d, want 1", f.binaryHit)
	}
	if !strings.Contains(r.out, "multica login") {
		t.Fatalf("next step (multica login) not printed:\n%s", r.out)
	}
}

func TestInstallScriptNeverExposesToken(t *testing.T) {
	f := newInstallFake(t, fakeBinary(installTestCommit[:7]), "")
	r := runInstall(t, f, installTestToken, "")
	if r.err != nil {
		t.Fatalf("install failed: %v\n%s", r.err, r.out)
	}
	if r.argv == "" {
		t.Fatal("curl wrapper recorded no argv; test is not observing curl")
	}
	if strings.Contains(r.argv, installTestToken) || strings.Contains(r.argv, "Bearer") {
		t.Fatalf("token reached curl argv:\n%s", r.argv)
	}
	if strings.Contains(r.out, installTestToken) {
		t.Fatalf("token printed:\n%s", r.out)
	}
}

func TestInstallScriptDigestMismatchAborts(t *testing.T) {
	f := newInstallFake(t, fakeBinary(installTestCommit[:7]), strings.Repeat("0", 64))
	r := runInstall(t, f, installTestToken, "OLD")
	if r.err == nil {
		t.Fatalf("digest mismatch must fail:\n%s", r.out)
	}
	if !strings.Contains(r.out, "sha256") {
		t.Fatalf("failure must name sha256:\n%s", r.out)
	}
	if b, _ := os.ReadFile(r.bin); string(b) != "OLD" {
		t.Fatalf("installed binary was replaced: %q", b)
	}
}

func TestInstallScriptProbeCommitMismatchAborts(t *testing.T) {
	f := newInstallFake(t, fakeBinary("deadbee"), "")
	r := runInstall(t, f, installTestToken, "OLD")
	if r.err == nil {
		t.Fatalf("wrong-commit candidate must fail:\n%s", r.out)
	}
	if b, _ := os.ReadFile(r.bin); string(b) != "OLD" {
		t.Fatalf("installed binary was replaced: %q", b)
	}
}

// A candidate that prints the expected commit but exits non-zero is broken.
func TestInstallScriptProbeNonZeroExitAborts(t *testing.T) {
	bad := fakeBinary(installTestCommit[:7]) + "exit 1\n"
	f := newInstallFake(t, bad, "")
	r := runInstall(t, f, installTestToken, "OLD")
	if r.err == nil {
		t.Fatalf("a candidate whose --version exits 1 must fail:\n%s", r.out)
	}
	if b, _ := os.ReadFile(r.bin); string(b) != "OLD" {
		t.Fatalf("installed binary was replaced: %q", b)
	}
}

// A candidate that prints the commit and then hangs must not stall the
// installer or replace the installed CLI.
func TestInstallScriptProbeTimeoutAborts(t *testing.T) {
	hang := fakeBinary(installTestCommit[:7]) + "sleep 30\n"
	f := newInstallFake(t, hang, "")
	start := time.Now()
	r := runInstall(t, f, installTestToken, "OLD", "MULTICA_PROBE_TIMEOUT=1")
	if r.err == nil {
		t.Fatalf("a hanging --version probe must fail:\n%s", r.out)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("probe was not bounded: install took %s", took)
	}
	if b, _ := os.ReadFile(r.bin); string(b) != "OLD" {
		t.Fatalf("installed binary was replaced: %q", b)
	}
}

// The printed next step must not tell the user to put a token in argv.
func TestInstallScriptLoginHintHasNoTokenArgument(t *testing.T) {
	f := newInstallFake(t, fakeBinary(installTestCommit[:7]), "")
	r := runInstall(t, f, installTestToken, "")
	if r.err != nil {
		t.Fatalf("install failed: %v\n%s", r.err, r.out)
	}
	if strings.Contains(r.out, "<YOUR_TOKEN>") || strings.Contains(r.out, installTestToken) {
		t.Fatalf("login hint carries a token argument:\n%s", r.out)
	}
	var hint string
	for _, line := range strings.Split(r.out, "\n") {
		if strings.Contains(line, "multica login") {
			hint = strings.TrimSpace(line)
		}
	}
	if hint != "multica login --token" {
		t.Fatalf("login hint = %q, want exactly %q (prompts, no argv token)", hint, "multica login --token")
	}
}

func TestInstallScriptWithoutTokenOrTTYFails(t *testing.T) {
	f := newInstallFake(t, fakeBinary(installTestCommit[:7]), "")
	r := runInstall(t, f, "", "")
	if r.err == nil || !strings.Contains(r.out, "MULTICA_TOKEN") {
		t.Fatalf("expected failure naming MULTICA_TOKEN, err=%v:\n%s", r.err, r.out)
	}
	if f.binaryHit != 0 {
		t.Fatal("binary was requested without a token")
	}
}

func TestInstallScriptWrongTokenFails(t *testing.T) {
	f := newInstallFake(t, fakeBinary(installTestCommit[:7]), "")
	r := runInstall(t, f, "mul_wrong", "OLD")
	if r.err == nil {
		t.Fatalf("rejected token must fail:\n%s", r.out)
	}
	if strings.Contains(r.out, "mul_wrong") {
		t.Fatalf("token printed:\n%s", r.out)
	}
}

func TestInstallScriptRejectsUnsafeHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/daemon/install.sh", nil)
	req.Host = "x';touch /tmp/pwned;'"
	w := httptest.NewRecorder()
	DaemonInstallScript(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unsafe host: status %d, want 400", w.Code)
	}
	if strings.Contains(w.Body.String(), "pwned") {
		t.Fatal("unsafe host reached the script body")
	}
}
