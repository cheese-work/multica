package sandbox

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary double as the in-sandbox bridge helper.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == BridgeHelperArg {
		os.Exit(BridgeMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// fakeEgress is a host-side stand-in for the egress proxy: a Unix socket
// that answers each connection with "pong:<first line>".
func fakeEgress(t *testing.T) (sock string, seen chan string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "egress")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "egress.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	seen = make(chan string, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				if line == "" {
					return // the bridge's start-up probe
				}
				seen <- strings.TrimSpace(line)
				c.Write([]byte("pong:" + line))
			}()
		}
	}()
	return sock, seen
}

func bridgeLauncher() *Launcher {
	l := newLauncher()
	l.BridgeExe = os.Args[0]
	return l
}

// viaProxy is a bash fake agent: it talks to whatever HTTPS_PROXY names.
const viaProxy = `hp=${HTTPS_PROXY#http://}; exec 3<>/dev/tcp/${hp%:*}/${hp#*:} && echo "CONNECT x:443" >&3 && read -r reply <&3 && echo "got=$reply"`

func TestEgressReachesHostProxyThroughLoopbackBridge(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	sock, seen := fakeEgress(t)
	ly := newLayout(t)
	spec := ly.bash(viaProxy)
	spec.EgressSocket = sock
	out, err := runSandbox(t, l, spec)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "got=pong:CONNECT x:443") {
		t.Errorf("agent did not reach the host proxy:\n%s", out)
	}
	select {
	case line := <-seen:
		if line != "CONNECT x:443" {
			t.Errorf("proxy saw %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Error("host proxy saw nothing")
	}
}

func TestEgressSetsProxyEnvAndOverridesCallerValues(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	sock, _ := fakeEgress(t)
	ly := newLayout(t)
	spec := ly.spec(`echo "$HTTPS_PROXY $HTTP_PROXY $https_proxy $http_proxy"`)
	spec.Env = append(spec.Env, "HTTPS_PROXY=http://evil.example:8080", "http_proxy=http://evil.example:8080")
	spec.EgressSocket = sock
	out, err := runSandbox(t, l, spec)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	want := "http://127.0.0.1:3128 http://127.0.0.1:3128 http://127.0.0.1:3128 http://127.0.0.1:3128"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("proxy env = %q, want %q", got, want)
	}
}

func TestEgressHostLoopbackStaysUnreachableDirectly(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	sock, _ := fakeEgress(t)
	host, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	port := host.Addr().(*net.TCPAddr).Port
	ly := newLayout(t)
	spec := ly.bash(`exec 3<>/dev/tcp/127.0.0.1/` + strconv.Itoa(port) + ` && echo REACHED || echo refused`)
	spec.EgressSocket = sock
	out, _ := runSandbox(t, l, spec)
	if !strings.Contains(out, "refused") || strings.Contains(out, "REACHED") {
		t.Errorf("host loopback port reachable from the netns:\n%s", out)
	}
}

func TestEgressMissingSocketStopsRunBeforeAgent(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	ly := newLayout(t)
	marker := filepath.Join(ly.work, "agent-ran")
	spec := ly.spec("touch " + marker)
	spec.EgressSocket = filepath.Join(ly.root, "nope", "egress.sock")
	out, err := runSandbox(t, l, spec)
	if err == nil {
		t.Errorf("run with no egress socket succeeded:\n%s", out)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("agent ran without a bridge: host-network or no-bridge fallback")
	}
}

func TestEgressDeadSocketStopsRunBeforeAgent(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	ly := newLayout(t)
	dir, err := os.MkdirTemp("", "egress")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "egress.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // the socket file stays, nobody listens
	marker := filepath.Join(ly.work, "agent-ran")
	spec := ly.spec("touch " + marker)
	spec.EgressSocket = sock
	out, runErr := runSandbox(t, l, spec)
	if runErr == nil {
		t.Errorf("run with a dead egress socket succeeded:\n%s", out)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("agent ran although the bridge could not reach the proxy")
	}
}

func TestEgressPropagatesAgentExitCode(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	sock, _ := fakeEgress(t)
	ly := newLayout(t)
	spec := ly.spec("exit 7")
	spec.EgressSocket = sock
	_, err := runSandbox(t, l, spec)
	if err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Errorf("err = %v, want exit status 7", err)
	}
}

func TestEgressModeOffAndAuditAddNoProxy(t *testing.T) {
	for _, mode := range []Mode{ModeOff, ModeAudit} {
		l := bridgeLauncher()
		l.Mode = mode
		ly := newLayout(t)
		spec := ly.spec(`echo "[$HTTPS_PROXY]"`)
		spec.EgressSocket = filepath.Join(ly.root, "nope.sock")
		out, err := runSandbox(t, l, spec)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if strings.Contains(out, "127.0.0.1") {
			t.Errorf("%s mode set proxy env: %s", mode, out)
		}
	}
}

func TestEgressArgsBindSocketDirAndBridgeReadOnly(t *testing.T) {
	l := bridgeLauncher()
	sock, _ := fakeEgress(t)
	ly := newLayout(t)
	spec := ly.spec("true")
	spec.EgressSocket = sock
	args, err := l.Args(spec)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--ro-bind " + filepath.Dir(sock) + " /run/multica-egress",
		"--ro-bind " + os.Args[0] + " /run/multica-bridge",
		"-- /run/multica-bridge " + BridgeHelperArg,
		"/run/multica-egress/egress.sock -- /bin/sh -c",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
}

func TestEgressArgsRejectUnsafeSockets(t *testing.T) {
	l := bridgeLauncher()
	sock, _ := fakeEgress(t)
	ly := newLayout(t)
	for name, mod := range map[string]func(*Spec){
		"relative":         func(s *Spec) { s.EgressSocket = "egress.sock" },
		"dir over workdir": func(s *Spec) { s.EgressSocket = filepath.Join(ly.root, "egress.sock") },
		"docker socket":    func(s *Spec) { s.EgressSocket = "/var/run/docker.sock" },
	} {
		spec := ly.spec("true")
		spec.EgressSocket = sock
		mod(&spec)
		if args, err := l.Args(spec); err == nil {
			t.Errorf("%s accepted: %v", name, args)
		}
	}
}

func TestBridgeRelaysAndRefusesNonLoopbackListen(t *testing.T) {
	sock, _ := fakeEgress(t)
	if code := BridgeMain([]string{"-listen", "0.0.0.0:0", "-unix", sock, "--", "/bin/true"}); code == 0 {
		t.Error("bridge listened on a non-loopback address")
	}
	if code := BridgeMain([]string{"-listen", "127.0.0.1:0", "-unix", sock, "--", "/bin/sh", "-c", "exit 3"}); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

// bash is spec with bash as the interpreter, for /dev/tcp.
func (l layout) bash(script string) Spec {
	s := l.spec("")
	s.Argv = []string{"/bin/bash", "-c", script}
	return s
}
