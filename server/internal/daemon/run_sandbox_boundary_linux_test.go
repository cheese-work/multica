//go:build linux

package daemon

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// CHE-6505 (A4d-7a): boundary evidence for a real runTask under enforcement
// on. A test-created fake claude runs through runTask and the real bwrap
// launcher, records what it can reach, and reports it in its result line.
// Every sentinel is checked by the absolute path of the test HOME, never
// through $HOME: applyRestrictedHome already changes $HOME under on, so a
// $HOME-relative check would pass without bwrap.

// boundaryClaude prints one line of observations. It never prints a value of
// GH_TOKEN, only whether it is set.
const boundaryClaude = `#!/bin/sh
case "$*" in *--version*) echo 9.9.9; exit 0;; esac
IFS= read -r _
direct=blocked
for a in $DIRECT_ADDRS; do
  if nc -z -w 3 "${a%:*}" "${a##*:}" >/dev/null 2>&1; then direct=reachable; fi
done
if [ -e "$SENTINEL" ]; then sentinel=present; else sentinel=absent; fi
if [ -n "${GH_TOKEN+x}" ]; then gh=set; else gh=unset; fi
ca="${TMPDIR:-/tmp}/ca.pem"
printf '%s\n' "$CA_PEM" > "$ca"
provider=$(curl -sS --max-time 15 --cacert "$ca" "$PROVIDER_URL" 2>/dev/null || echo failed)
printf '%s\n' '{"type":"system","session_id":"s1"}'
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","result":"direct=%s sentinel=%s gh=%s provider=%s"}\n' "$direct" "$sentinel" "$gh" "$provider"
`

// hangClaude starts a process outside its own process group and session, then
// keeps running until the run is cancelled.
const hangClaude = `#!/bin/sh
case "$*" in *--version*) echo 9.9.9; exit 0;; esac
setsid sleep "$TREE_MARK" &
printf '%s\n' '{"type":"system","session_id":"s1"}'
while :; do sleep 1; done
`

// boundaryProbe is what one fake claude run reported plus the daemon-side view.
type boundaryProbe struct {
	comment     string // the fake's result line
	log         string // the task log, which carries the proxy destination list
	providerHit int32  // requests the fake provider received
	port        string // port of the host test server
}

// hostTestServer is a TLS server on every host interface. It stands in for
// both a host service and the model provider: the run must reach it only
// through the proxy.
func hostTestServer(t *testing.T, hits *int32) (srv *httptest.Server, port string, caPEM string, directAddrs string) {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(hits, 1)
		_, _ = w.Write([]byte("pong"))
	}))
	srv.Listener = ln
	srv.StartTLS()
	t.Cleanup(srv.Close)
	port = strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	addrs := []string{"127.0.0.1:" + port}
	if ifs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				addrs = append(addrs, net.JoinHostPort(ipn.IP.String(), port))
				break
			}
		}
	}
	return srv, port, caPEM, strings.Join(addrs, " ")
}

// runBoundaryProbe runs boundaryClaude through the real runTask under mode.
func runBoundaryProbe(t *testing.T, mode sandbox.Mode, l runLauncher) boundaryProbe {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// Not a real credential: the test only asks whether the variable is set.
	t.Setenv("GH_TOKEN", "boundary-test-placeholder")
	sentinel := filepath.Join(home, ".config", "github-apps", "app.pem")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("placeholder, not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var hits int32
	_, port, caPEM, direct := hostTestServer(t, &hits)
	// The proxy dials the host loopback origin; production never sets this.
	runEgressAllowLoopback = true
	t.Cleanup(func() { runEgressAllowLoopback = false })

	d, task, _ := launchDaemon(t, mode, l)
	// daemon.New turns the inherited-env allowlist on under enforcement on; a
	// Daemon literal skips New, so mirror it.
	agent.SetRestrictedEnv(mode == sandbox.ModeOn)
	t.Cleanup(func() { agent.SetRestrictedEnv(false) })
	fake := filepath.Join(t.TempDir(), "claude")
	writeTestExecutable(t, fake, []byte(boundaryClaude))
	d.cfg.Agents = map[string]AgentEntry{"claude": {Path: fake}}
	task.Agent.CustomEnv = map[string]string{
		"SENTINEL": sentinel, "DIRECT_ADDRS": direct, "CA_PEM": caPEM,
		"PROVIDER_URL": "https://127.0.0.1:" + port + "/v1/ping",
	}
	logger, buf := egressTestLog()
	res, err := d.runTask(context.Background(), task, "claude", 0, logger)
	if err != nil || res.Status != "completed" {
		t.Fatalf("runTask = %+v, %v\nlog:\n%s", res, err, buf.String())
	}
	return boundaryProbe{comment: res.Comment, log: buf.String(), providerHit: atomic.LoadInt32(&hits), port: port}
}

// AC 1 to 3: under on, the run has no direct network, no host credential, and
// reaches the provider only through the proxy.
func TestRunTaskOnSandboxBoundary(t *testing.T) {
	p := runBoundaryProbe(t, sandbox.ModeOn, hostSandbox(t))
	t.Logf("fake claude saw: %s", p.comment)
	if !strings.Contains(p.comment, "direct=blocked") {
		t.Errorf("AC1: a direct TCP connect to the host test server worked: %q", p.comment)
	}
	if !strings.Contains(p.comment, "sentinel=absent") {
		t.Errorf("AC2: the credential sentinel is readable by its absolute path: %q", p.comment)
	}
	if !strings.Contains(p.comment, "gh=unset") {
		t.Errorf("AC2: GH_TOKEN is set inside the run: %q", p.comment)
	}
	if !strings.Contains(p.comment, "provider=pong") || p.providerHit == 0 {
		t.Errorf("AC3: the provider request through HTTPS_PROXY did not succeed: %q (hits %d)", p.comment, p.providerHit)
	}
	if want := fmt.Sprintf("host=127.0.0.1 port=%s", p.port); !strings.Contains(p.log, "egress destination") || !strings.Contains(p.log, want) {
		t.Errorf("AC3: the proxy destination log does not name the provider (%s):\n%s", want, p.log)
	}
}

// marked returns the pids whose command line holds the marker argument.
func marked(mark string) []int {
	var pids []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmd, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err == nil && bytes.Contains(cmd, []byte("\x00"+mark+"\x00")) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// runCgroups lists the daemon's multica-run-* cgroup directories.
func runCgroups(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skipf("no cgroup v2 entry: %v", err)
	}
	rel, ok := strings.CutPrefix(strings.SplitN(string(b), "\n", 2)[0], "0::")
	if !ok {
		t.Skip("no cgroup v2 entry")
	}
	dirs, _ := filepath.Glob(filepath.Join("/sys/fs/cgroup", rel, "multica-run-*"))
	m := map[string]bool{}
	for _, d := range dirs {
		m[d] = true
	}
	return m
}

func pollUntil(d time.Duration, cond func() bool) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// AC 4: cancelling the task kills the whole tree, including a process that left
// the agent's process group and session, and the run cgroup is removed.
func TestRunTaskOnCancelKillsWholeTree(t *testing.T) {
	l := hostSandbox(t)
	before := runCgroups(t)
	mark := fmt.Sprintf("31337.%06d", time.Now().UnixNano()%1000000)
	t.Cleanup(func() {
		for _, pid := range marked(mark) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	d, task, _ := launchDaemon(t, sandbox.ModeOn, l)
	fake := filepath.Join(t.TempDir(), "claude")
	writeTestExecutable(t, fake, []byte(hangClaude))
	d.cfg.Agents = map[string]AgentEntry{"claude": {Path: fake}}
	task.Agent.CustomEnv = map[string]string{"TREE_MARK": mark}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := d.runTask(ctx, task, "claude", 0, quietLog())
		done <- err
	}()
	if !pollUntil(20*time.Second, func() bool { return len(marked(mark)) > 0 }) {
		t.Fatal("the fake claude never started its child process in the sandbox")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runTask did not return after cancel")
	}
	if !pollUntil(10*time.Second, func() bool { return len(marked(mark)) == 0 }) {
		t.Errorf("processes left after cancel: %v", marked(mark))
	}
	if !pollUntil(5*time.Second, func() bool {
		for d := range runCgroups(t) {
			if !before[d] {
				return false
			}
		}
		return true
	}) {
		t.Errorf("run cgroup left after cancel: %v", runCgroups(t))
	}
}
