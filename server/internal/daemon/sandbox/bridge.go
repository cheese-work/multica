package sandbox

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// The run's only network path (CHE-2216 slice A7a). The sandbox netns has
// loopback and nothing else. A bridge process inside it listens on
// 127.0.0.1:egressPort and relays each connection to the host egress proxy
// over a Unix socket bind-mounted from the host. The proxy variables point
// at that port; a client that ignores them finds no route.
const (
	// BridgeHelperArg is os.Args[1] of the in-sandbox bridge process.
	BridgeHelperArg = "__sandbox-bridge"

	egressPort   = "3128"
	egressDir    = "/run/multica-egress"
	brokerDir    = "/run/multica-broker"
	daemonDir    = "/run/multica-daemon"
	bridgeMount  = "/run/multica-bridge"
	exitNoBridge = 125 // the bridge could not start; the agent never ran
)

var proxyNames = []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"}

func proxyEnv() []string {
	env := make([]string, 0, len(proxyNames))
	for _, n := range proxyNames {
		env = append(env, n+"=http://127.0.0.1:"+egressPort)
	}
	return env
}

func isProxyVar(kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	for _, n := range proxyNames {
		if name == n {
			return true
		}
	}
	return false
}

func (l *Launcher) bridgeExe() string {
	if l.BridgeExe != "" {
		return l.BridgeExe
	}
	exe, _ := os.Executable()
	return exe
}

// checkSocket refuses a socket path the bridge cannot be built around. It
// runs again at launch; a failure there stops the run (no fallback).
func checkSocket(what, sock string) error {
	if !filepath.IsAbs(sock) || filepath.Clean(sock) != sock {
		return fmt.Errorf("sandbox: %s socket %q is not absolute and clean", what, sock)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		return fmt.Errorf("sandbox: %s socket: %w", what, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("sandbox: %s socket %s is not a Unix socket", what, sock)
	}
	return nil
}

// checkDaemonPort refuses a daemon port the bridge cannot listen on.
func checkDaemonPort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || port != strconv.Itoa(n) {
		return fmt.Errorf("sandbox: daemon port %q is not a TCP port", port)
	}
	if port == egressPort {
		return fmt.Errorf("sandbox: daemon port %s collides with the egress proxy port", port)
	}
	return nil
}

// command is the argv bwrap executes: the agent itself, or the bridge that
// starts it once the network path is up.
func (s Spec) command() []string {
	if s.EgressSocket == "" && s.DaemonSocket == "" {
		return s.Argv
	}
	argv := []string{bridgeMount, BridgeHelperArg}
	if s.EgressSocket != "" {
		argv = append(argv, "-listen", "127.0.0.1:"+egressPort,
			"-unix", egressDir+"/"+filepath.Base(s.EgressSocket))
	}
	if s.DaemonSocket != "" {
		argv = append(argv, "-daemon-listen", "127.0.0.1:"+s.DaemonPort,
			"-daemon-unix", daemonDir+"/"+filepath.Base(s.DaemonSocket))
	}
	argv = append(argv, "--")
	return append(argv, s.Argv...)
}

// BridgeMain is the in-sandbox bridge: it checks each host socket answers,
// listens on loopback for each, then runs the agent command with those
// listeners alive and returns the agent's exit code. Two forwards exist: the
// egress proxy (-listen/-unix) and the daemon API (-daemon-listen/-daemon-unix).
// At least one is required, and each flag pair is all or nothing. Any failure
// before the agent starts returns exitNoBridge without running it.
func BridgeMain(args []string) int {
	fs := flag.NewFlagSet(BridgeHelperArg, flag.ContinueOnError)
	listen := fs.String("listen", "", "loopback host:port")
	sock := fs.String("unix", "", "egress proxy Unix socket")
	dListen := fs.String("daemon-listen", "", "loopback host:port for the daemon API")
	dSock := fs.String("daemon-unix", "", "daemon API Unix socket")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		return exitNoBridge
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "sandbox bridge:", err)
		return exitNoBridge
	}
	var lns []net.Listener
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()
	forwards := 0
	for _, f := range []struct{ name, listen, sock string }{
		{"egress proxy", *listen, *sock}, {"daemon API", *dListen, *dSock},
	} {
		if f.listen == "" && f.sock == "" {
			continue
		}
		if f.listen == "" || f.sock == "" {
			return fail(fmt.Errorf("%s forward needs both a listen address and a socket", f.name))
		}
		forwards++
		host, _, err := net.SplitHostPort(f.listen)
		if err != nil {
			return fail(err)
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fail(errors.New("listen address is not loopback"))
		}
		probe, err := net.Dial("unix", f.sock)
		if err != nil {
			return fail(fmt.Errorf("%s unreachable: %w", f.name, err))
		}
		probe.Close()
		ln, err := net.Listen("tcp", f.listen)
		if err != nil {
			return fail(err)
		}
		lns = append(lns, ln)
		go serve(ln, f.sock)
	}
	if forwards == 0 {
		return fail(errors.New("no forward requested"))
	}
	return runAgent(fs.Args())
}

func serve(ln net.Listener, sock string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go relay(c, sock)
	}
}

// relay joins one client connection to a fresh connection to the proxy.
func relay(client net.Conn, sock string) {
	defer client.Close()
	up, err := net.Dial("unix", sock)
	if err != nil {
		return
	}
	defer up.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	go pipe(up, client)
	go pipe(client, up)
	wg.Wait()
}

// runAgent runs argv with the bridge's stdio, forwards termination signals,
// and maps its exit to the bridge's exit code.
func runAgent(argv []string) int {
	// Pdeathsig fires when the thread that forked the agent exits.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	dieWithBridge(cmd)
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox bridge:", err)
		return exitNoBridge + 1
	}
	go func() {
		for sig := range sigs {
			_ = cmd.Process.Signal(sig)
		}
	}()
	_ = cmd.Wait() // the exit status is read from ProcessState
	return exitCode(cmd.ProcessState)
}
