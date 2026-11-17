package sandbox

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDaemonAPI is a host-side stand-in for the run's daemon socket: it
// answers every request with "daemon:<method> <path>".
func fakeDaemonAPI(t *testing.T) (sock string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "dapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "daemon:"+r.Method+" "+r.URL.Path)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

// postCheckout is a bash fake agent: a raw HTTP POST to the daemon port.
const postCheckout = `exec 3<>/dev/tcp/127.0.0.1/$MULTICA_DAEMON_PORT && printf 'POST /repo/checkout HTTP/1.0\r\nHost: x\r\nContent-Length: 0\r\n\r\n' >&3 && cat <&3`

// AC 1: a POST to 127.0.0.1:$MULTICA_DAEMON_PORT inside the netns reaches the
// daemon socket.
func TestDaemonAPIReachesHostSocketThroughLoopbackBridge(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	sock := fakeDaemonAPI(t)
	ly := newLayout(t)
	spec := ly.bash(postCheckout)
	spec.Env = append(spec.Env, "MULTICA_DAEMON_PORT=19514")
	spec.DaemonSocket, spec.DaemonPort = sock, "19514"
	out, err := runSandbox(t, l, spec)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon:POST /repo/checkout") {
		t.Errorf("agent did not reach the daemon socket:\n%s", out)
	}
}

// Both forwards at once: the daemon API and the egress proxy each reach their
// own host socket.
func TestDaemonAPIAndEgressForwardTogether(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	egress, _ := fakeEgress(t)
	ly := newLayout(t)
	spec := ly.bash(postCheckout + `; ` + viaProxy)
	spec.DaemonSocket, spec.DaemonPort, spec.EgressSocket = fakeDaemonAPI(t), "19514", egress
	out, err := runSandbox(t, l, spec)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	for _, want := range []string{"daemon:POST /repo/checkout", "got=pong:CONNECT x:443"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// AC 3: with no daemon socket, or a dead one, the agent never starts.
func TestDaemonAPIMissingOrDeadSocketStopsRunBeforeAgent(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	dead := func(t *testing.T) string {
		dir, err := os.MkdirTemp("", "dapi")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		sock := filepath.Join(dir, "daemon.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		ln.Close() // the socket file stays, nobody listens
		return sock
	}
	for name, sockFor := range map[string]func(*testing.T, layout) string{
		"missing": func(_ *testing.T, ly layout) string { return filepath.Join(ly.root, "nope", "daemon.sock") },
		"dead":    func(t *testing.T, _ layout) string { return dead(t) },
	} {
		ly := newLayout(t)
		marker := filepath.Join(ly.work, "agent-ran")
		spec := ly.spec("touch " + marker)
		spec.DaemonSocket, spec.DaemonPort = sockFor(t, ly), "19514"
		out, err := runSandbox(t, l, spec)
		if err == nil {
			t.Errorf("%s: run succeeded:\n%s", name, out)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Errorf("%s: agent ran without a daemon forwarder", name)
		}
	}
}

// AC 4: a run binds only its own socket directory. Run B's socket is not
// visible in run A's sandbox, so A cannot reach it.
func TestDaemonAPIRunSeesOnlyItsOwnSocketDir(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	a, b := fakeDaemonAPI(t), fakeDaemonAPI(t)
	ly := newLayout(t)
	spec := ly.spec(`ls /run/multica-daemon; test -e ` + b + ` && echo B-VISIBLE || echo b-absent`)
	spec.DaemonSocket, spec.DaemonPort = a, "19514"
	out, err := runSandbox(t, l, spec)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon.sock") || !strings.Contains(out, "b-absent") || strings.Contains(out, "B-VISIBLE") {
		t.Errorf("sandbox mounts more than its own socket dir:\n%s", out)
	}
}

func TestDaemonAPIArgsBindDirAndForwardFlags(t *testing.T) {
	l := bridgeLauncher()
	sock := fakeDaemonAPI(t)
	ly := newLayout(t)
	spec := ly.spec("true")
	spec.DaemonSocket, spec.DaemonPort = sock, "19514"
	spec.Env = append(spec.Env, "MULTICA_DAEMON_PORT=1")
	args, err := l.Args(spec)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--ro-bind " + filepath.Dir(sock) + " /run/multica-daemon",
		"--ro-bind " + os.Args[0] + " /run/multica-bridge",
		"-daemon-listen 127.0.0.1:19514 -daemon-unix /run/multica-daemon/daemon.sock -- /bin/sh -c",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "-listen 127.0.0.1:3128") {
		t.Errorf("daemon-only spec forwards the egress port:\n%s", joined)
	}
	// the run's port is set from the spec, whatever the caller passed
	if got := strings.Count(strings.Join(spec.environ(), "\n"), "MULTICA_DAEMON_PORT="); got != 1 {
		t.Errorf("MULTICA_DAEMON_PORT appears %d times, want 1", got)
	}
}

func TestDaemonAPIArgsRejectUnsafeSpecs(t *testing.T) {
	l := bridgeLauncher()
	sock := fakeDaemonAPI(t)
	ly := newLayout(t)
	for name, mod := range map[string]func(*Spec){
		"relative socket": func(s *Spec) { s.DaemonSocket = "daemon.sock" },
		"no port":         func(s *Spec) { s.DaemonPort = "" },
		"bad port":        func(s *Spec) { s.DaemonPort = "70000" },
		"egress port":     func(s *Spec) { s.DaemonPort = egressPort },
		"dir over work":   func(s *Spec) { s.DaemonSocket = filepath.Join(ly.root, "daemon.sock") },
	} {
		spec := ly.spec("true")
		spec.DaemonSocket, spec.DaemonPort = sock, "19514"
		mod(&spec)
		if args, err := l.Args(spec); err == nil {
			t.Errorf("%s accepted: %v", name, args)
		}
	}
}

func TestBridgeRefusesHalfAForwardPairOrNone(t *testing.T) {
	sock := fakeDaemonAPI(t)
	for name, args := range map[string][]string{
		"none":         {"--", "/bin/true"},
		"listen only":  {"-daemon-listen", "127.0.0.1:0", "--", "/bin/true"},
		"unix only":    {"-daemon-unix", sock, "--", "/bin/true"},
		"non-loopback": {"-daemon-listen", "0.0.0.0:0", "-daemon-unix", sock, "--", "/bin/true"},
	} {
		if code := BridgeMain(args); code == 0 {
			t.Errorf("%s: bridge started", name)
		}
	}
	if code := BridgeMain([]string{"-daemon-listen", "127.0.0.1:0", "-daemon-unix", sock, "--", "/bin/sh", "-c", "exit 4"}); code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
}
