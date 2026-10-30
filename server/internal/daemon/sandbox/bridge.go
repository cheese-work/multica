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

// checkEgress refuses a socket path the bridge cannot be built around. It
// runs again at launch; a failure there stops the run (no fallback).
func (l *Launcher) checkEgress(sock string) error {
	if !filepath.IsAbs(sock) || filepath.Clean(sock) != sock {
		return fmt.Errorf("sandbox: egress socket %q is not absolute and clean", sock)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		return fmt.Errorf("sandbox: egress socket: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("sandbox: egress socket %s is not a Unix socket", sock)
	}
	return nil
}

// command is the argv bwrap executes: the agent itself, or the bridge that
// starts it once the network path is up.
func (s Spec) command() []string {
	if s.EgressSocket == "" {
		return s.Argv
	}
	argv := []string{bridgeMount, BridgeHelperArg,
		"-listen", "127.0.0.1:" + egressPort,
		"-unix", egressDir + "/" + filepath.Base(s.EgressSocket), "--"}
	return append(argv, s.Argv...)
}

// BridgeMain is the in-sandbox bridge: it checks the proxy socket answers,
// listens on loopback, then runs the agent command with that listener alive
// and returns the agent's exit code. Any failure before the agent starts
// returns exitNoBridge without running it.
func BridgeMain(args []string) int {
	fs := flag.NewFlagSet(BridgeHelperArg, flag.ContinueOnError)
	listen := fs.String("listen", "", "loopback host:port")
	sock := fs.String("unix", "", "egress proxy Unix socket")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		return exitNoBridge
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "sandbox bridge:", err)
		return exitNoBridge
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fail(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fail(errors.New("listen address is not loopback"))
	}
	probe, err := net.Dial("unix", *sock)
	if err != nil {
		return fail(fmt.Errorf("egress proxy unreachable: %w", err))
	}
	probe.Close()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fail(err)
	}
	defer ln.Close()
	go serve(ln, *sock)
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
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
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
			cmd.Process.Signal(sig)
		}
	}()
	cmd.Wait()
	return exitCode(cmd.ProcessState)
}
