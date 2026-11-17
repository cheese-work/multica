package daemon

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// syncBuf is a log sink the proxy goroutines and the test share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func egressTestLog() (*slog.Logger, *syncBuf) {
	buf := &syncBuf{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// connectViaSocket opens a CONNECT tunnel to hostport through the run socket.
func connectViaSocket(t *testing.T, sock, hostport string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostport, hostport)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s: status %d", hostport, resp.StatusCode)
	}
	return c
}

func TestRunEgress_TunnelsToTLSOriginAndRemovesSocket(t *testing.T) {
	runEgressAllowLoopback = true
	t.Cleanup(func() { runEgressAllowLoopback = false })
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "pong") }))
	t.Cleanup(origin.Close)
	u, _ := url.Parse(origin.URL)
	log, _ := egressTestLog()

	re, err := startRunEgress(sandbox.ModeOn, []string{"api.anthropic.com"}, log)
	if err != nil || re == nil {
		t.Fatalf("startRunEgress = %v, %v; want a proxy", re, err)
	}
	dir := filepath.Dir(re.Socket)
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("run dir %s: %v, %v; want mode 0700", dir, fi, err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	tc := tls.Client(connectViaSocket(t, re.Socket, u.Host), &tls.Config{RootCAs: pool, ServerName: "example.com"})
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err != nil {
		// httptest's cert is valid for example.com and 127.0.0.1.
		t.Fatalf("TLS through the tunnel: %v", err)
	}
	fmt.Fprintf(tc, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	body, _ := io.ReadAll(tc)
	if !strings.Contains(string(body), "pong") {
		t.Fatalf("response through tunnel = %q; want pong", body)
	}

	re.Close()
	re.Close() // safe on repeat
	if _, err := os.Stat(re.Socket); !os.IsNotExist(err) {
		t.Fatalf("socket after Close: %v; want not-exist", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("run dir after Close: %v; want not-exist", err)
	}
}

func TestRunEgress_FirstExternalDestinationLoggedOnce(t *testing.T) {
	runEgressAllowLoopback = true
	t.Cleanup(func() { runEgressAllowLoopback = false })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	log, buf := egressTestLog()
	re, err := startRunEgress(sandbox.ModeOn, []string{"api.anthropic.com"}, log)
	if err != nil || re == nil {
		t.Fatalf("startRunEgress = %v, %v", re, err)
	}
	defer re.Close()
	connectViaSocket(t, re.Socket, ln.Addr().String())
	connectViaSocket(t, re.Socket, ln.Addr().String())
	re.Close() // writes the destination list
	out := buf.String()
	if n := strings.Count(out, "external read"); n != 1 {
		t.Fatalf("external read logged %d times, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "egress destination"); n != 2 {
		t.Fatalf("destination list has %d entries, want 2:\n%s", n, out)
	}
}

func TestRunEgress_TrustedDestinationIsNotExternal(t *testing.T) {
	runEgressAllowLoopback = true
	t.Cleanup(func() { runEgressAllowLoopback = false })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	log, buf := egressTestLog()
	re, err := startRunEgress(sandbox.ModeOn, []string{"127.0.0.1"}, log)
	if err != nil || re == nil {
		t.Fatalf("startRunEgress = %v, %v", re, err)
	}
	connectViaSocket(t, re.Socket, ln.Addr().String())
	re.Close()
	if strings.Contains(buf.String(), "external read") {
		t.Fatalf("trusted host logged as external read:\n%s", buf.String())
	}
}

func TestRunEgress_OffAndAuditStartNothing(t *testing.T) {
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "mre-*"))
	for _, m := range []sandbox.Mode{sandbox.ModeOff, sandbox.ModeAudit} {
		log, _ := egressTestLog()
		re, err := startRunEgress(m, nil, log)
		if re != nil || err != nil {
			t.Fatalf("mode %s: got %v, %v; want nil, nil", m, re, err)
		}
		re.Close() // nil-safe
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "mre-*"))
	if len(after) != len(before) {
		t.Fatalf("off/audit created run dirs: %v -> %v", before, after)
	}
}

func TestEgressTrustedHosts(t *testing.T) {
	cases := []struct {
		server, provider string
		want             []string
	}{
		{"wss://multica.example.com:8443/ws", "claude", []string{"multica.example.com", "api.anthropic.com"}},
		{"http://localhost:8080", "codex", []string{"localhost", "api.openai.com", "chatgpt.com"}},
		{"https://m.example", "gemini", []string{"m.example"}},
		{"", "claude", []string{"api.anthropic.com"}},
	}
	for _, c := range cases {
		got := strings.Join(egressTrustedHosts(c.server, c.provider), ",")
		if got != strings.Join(c.want, ",") {
			t.Errorf("egressTrustedHosts(%q, %q) = %s; want %s", c.server, c.provider, got, strings.Join(c.want, ","))
		}
	}
}

// readEnds reports whether c is closed by the other side within d.
func readEnds(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

// Close must end every connection the proxy accepted and wait for its handler,
// so the logged destination list is complete (CHE-6502 review round 1).
func TestRunEgress_CloseEndsHeldConnectionsBeforeLogging(t *testing.T) {
	runEgressAllowLoopback = true
	t.Cleanup(func() { runEgressAllowLoopback = false })
	held := make(chan net.Conn, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err == nil {
			held <- c // never closed by the origin: the tunnel stays open
		}
	}()
	log, buf := egressTestLog()
	re, err := startRunEgress(sandbox.ModeOn, nil, log)
	if err != nil || re == nil {
		t.Fatalf("startRunEgress = %v, %v", re, err)
	}
	tunnel := connectViaSocket(t, re.Socket, ln.Addr().String())
	<-held
	idle, err := net.Dial("unix", re.Socket) // accepted, no request sent yet
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idle.Close() })

	closed := make(chan struct{})
	go func() { re.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return with a tunnel and an idle connection held open")
	}
	if !readEnds(tunnel, time.Second) {
		t.Error("established tunnel still open after Close")
	}
	if !readEnds(idle, time.Second) {
		t.Error("idle connection still open after Close")
	}
	if n := strings.Count(buf.String(), "egress destination"); n != 1 {
		t.Errorf("destination list has %d entries, want 1:\n%s", n, buf.String())
	}
}
