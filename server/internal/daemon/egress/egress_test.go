package egress

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// origin is a TCP echo server standing in for a remote host.
func origin(t *testing.T) (port string) {
	t.Helper()
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
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port
}

// startProxy serves p on a Unix socket and returns a dialer for it.
func startProxy(t *testing.T, p *Proxy) func() net.Conn {
	t.Helper()
	dir, err := os.MkdirTemp("", "egress")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go p.Serve(ln)
	return func() net.Conn {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		t.Cleanup(func() { c.Close() })
		return c
	}
}

func status(t *testing.T, c net.Conn, req string) (int, *bufio.Reader) {
	t.Helper()
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, br
}

func connect(host, port string) string {
	a := net.JoinHostPort(host, port)
	return "CONNECT " + a + " HTTP/1.1\r\nHost: " + a + "\r\n\r\n"
}

func TestConnectTunnelsBytes(t *testing.T) {
	port := origin(t)
	dial := startProxy(t, &Proxy{AllowLoopback: true})
	c := dial()
	code, br := status(t, c, connect("127.0.0.1", port))
	if code != 200 {
		t.Fatalf("CONNECT: status %d, want 200", code)
	}
	io.WriteString(c, "hello\n")
	line, _ := br.ReadString('\n')
	if line != "hello\n" {
		t.Fatalf("echo through tunnel = %q", line)
	}
}

func TestNonConnectRefused(t *testing.T) {
	port := origin(t)
	dial := startProxy(t, &Proxy{AllowLoopback: true})
	for _, req := range []string{
		"GET http://127.0.0.1:" + port + "/ HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n", // absolute-form
		"GET / HTTP/1.1\r\nHost: 127.0.0.1:" + port + "\r\n\r\n",
		"POST http://127.0.0.1:" + port + "/ HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n",
	} {
		code, _ := status(t, dial(), req)
		if code != http.StatusMethodNotAllowed {
			t.Errorf("%q: status %d, want 405", strings.SplitN(req, "\r\n", 2)[0], code)
		}
	}
}

func TestRefusedDestinations(t *testing.T) {
	port := origin(t)
	cases := map[string]struct {
		p        *Proxy
		host, pt string
		want     int
	}{
		"ssh port":      {&Proxy{AllowLoopback: true}, "127.0.0.1", "22", 403},
		"policy deny":   {&Proxy{AllowLoopback: true, Allow: func(string, string) bool { return false }}, "127.0.0.1", port, 403},
		"host loopback": {&Proxy{}, "127.0.0.1", port, 502},
		"link-local":    {&Proxy{}, "169.254.169.254", "80", 502},
		"bad port":      {&Proxy{AllowLoopback: true}, "127.0.0.1", "x", 400},
	}
	for name, tc := range cases {
		code, _ := status(t, startProxy(t, tc.p)(), connect(tc.host, tc.pt))
		if code != tc.want {
			t.Errorf("%s: status %d, want %d", name, code, tc.want)
		}
	}
}

func TestDestinationLogAndExternalRead(t *testing.T) {
	port := origin(t)
	var fired atomic.Int32
	p := &Proxy{
		AllowLoopback:  true,
		Trusted:        []string{"127.0.0.1"}, // stands in for the Multica server / provider API
		OnExternalRead: func(string) { fired.Add(1) },
	}
	dial := startProxy(t, p)

	status(t, dial(), connect("127.0.0.1", port))
	if p.ExternalRead() || fired.Load() != 0 {
		t.Fatal("trusted destination marked external_read")
	}

	status(t, dial(), connect("localhost", port))
	if !p.ExternalRead() || fired.Load() != 1 {
		t.Fatalf("untrusted destination: ExternalRead=%v fired=%d", p.ExternalRead(), fired.Load())
	}
	status(t, dial(), connect("LOCALHOST", port))
	if fired.Load() != 1 {
		t.Fatalf("callback fired %d times, want once per task", fired.Load())
	}

	d := p.Dests()
	if len(d) != 3 || d[0].Host != "127.0.0.1" || d[0].External || !d[1].External || d[1].Host != "localhost" {
		t.Fatalf("destination log = %+v", d)
	}
}

func TestRefusedDestinationIsLogged(t *testing.T) {
	p := &Proxy{AllowLoopback: true, Allow: func(string, string) bool { return false }}
	status(t, startProxy(t, p)(), connect("blocked.example", "443"))
	d := p.Dests()
	if len(d) != 1 || d[0].Allowed || d[0].Host != "blocked.example" {
		t.Fatalf("destination log = %+v", d)
	}
}
