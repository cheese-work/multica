package egress

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	providerHost = "api.provider.test"
	multicaHost  = "multica.example.test"
	multicaPort  = "8443"
)

// fakeResolver answers from a map and counts lookups.
type fakeResolver struct {
	answers map[string][]netip.Addr
	err     error
	calls   atomic.Int32
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.answers[host], nil
}

// sink stands in for the remote host: it counts connections and the bytes
// that reached it, and echoes them back.
type sink struct {
	mu    sync.Mutex
	conns int
	got   []byte
	dials []string
}

func (s *sink) dial(_ context.Context, _, address string) (net.Conn, error) {
	s.mu.Lock()
	s.conns++
	s.dials = append(s.dials, address)
	s.mu.Unlock()
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		buf := make([]byte, 4096)
		for {
			n, err := server.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.got = append(s.got, buf[:n]...)
				s.mu.Unlock()
				server.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return client, nil
}

func (s *sink) received() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

var publicA = netip.MustParseAddr("203.0.113.10") // TEST-NET-3: public class, never dialled for real

type rig struct {
	p    *Proxy
	res  *fakeResolver
	sink *sink
	dial func() net.Conn
}

func newRig(t *testing.T, answers map[string][]netip.Addr, pinned ...netip.Addr) *rig {
	t.Helper()
	res := &fakeResolver{answers: answers}
	pol, err := NewElevatedPolicy(multicaHost, []string{"443", multicaPort}, []string{providerHost}, pinned, res)
	if err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	pol.dial = s.dial
	p := &Proxy{Elevated: pol, Trusted: []string{providerHost, multicaHost}}
	return &rig{p: p, res: res, sink: s, dial: startProxy(t, p)}
}

func defaultRig(t *testing.T) *rig {
	return newRig(t, map[string][]netip.Addr{
		providerHost: {publicA},
		multicaHost:  {netip.MustParseAddr("127.0.0.1")},
	}, netip.MustParseAddr("127.0.0.1"))
}

// --- ClientHello builders ---

type ext struct {
	typ  uint16
	body []byte
}

func u16(v int) []byte { return binary.BigEndian.AppendUint16(nil, uint16(v)) }

func sniExt(names ...string) ext {
	var list []byte
	for _, n := range names {
		list = append(list, 0)
		list = append(list, u16(len(n))...)
		list = append(list, n...)
	}
	return ext{0, append(u16(len(list)), list...)}
}

// hello builds one TLS record holding a ClientHello with the given extensions.
func hello(exts ...ext) []byte {
	var xs []byte
	for _, e := range exts {
		xs = append(xs, u16(int(e.typ))...)
		xs = append(xs, u16(len(e.body))...)
		xs = append(xs, e.body...)
	}
	body := []byte{3, 3}
	body = append(body, make([]byte, 32)...)
	body = append(body, 0)                // empty session id
	body = append(body, u16(2)...)        // one cipher suite
	body = append(body, 0x13, 0x01, 1, 0) // suite, one compression method (null)
	body = append(body, u16(len(xs))...)
	body = append(body, xs...)
	hs := append([]byte{1, 0, byte(len(body) >> 8), byte(len(body))}, body...)
	return append([]byte{0x16, 3, 1}, append(u16(len(hs)), hs...)...)
}

// realHello captures what crypto/tls sends first for the given server name.
func realHello(t *testing.T, serverName string) []byte {
	t.Helper()
	client, server := net.Pipe()
	defer server.Close()
	go func() {
		c := tls.Client(client, &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
		c.Handshake()
		client.Close()
	}()
	buf := make([]byte, 16384)
	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := server.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

func rawConnect(authority string) string {
	return "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n\r\n"
}

// expectClosedWithoutUpstreamBytes sends a hello after a 200 and asserts the
// proxy hangs up and nothing reaches the remote host.
func expectHelloRefused(t *testing.T, r *rig, host string, hello []byte) {
	t.Helper()
	c := r.dial()
	code, br := status(t, c, connect(host, "443"))
	if code != 200 {
		t.Fatalf("CONNECT %s: status %d, want 200 before the hello", host, code)
	}
	c.Write(hello)
	if _, err := br.ReadByte(); err == nil {
		t.Fatalf("hello refused case: proxy answered instead of closing")
	}
	if n := r.sink.received(); n != 0 {
		t.Fatalf("%d bytes reached the remote host", n)
	}
	d := r.p.Dests()
	last := d[len(d)-1]
	if last.Allowed || last.Reason == "" {
		t.Fatalf("refused hello not logged: %+v", last)
	}
}

// --- T17 (a)-(c): hostname-only CONNECT, exact allowlist ---

func TestElevatedAllowedHostTunnelsMatchingSNI(t *testing.T) {
	for _, tc := range []struct{ name, authority, sni string }{
		{"provider", providerHost, providerHost},
		{"upper and trailing dot", "API.Provider.TEST.", providerHost},
		{"upper-case sni", providerHost, "API.PROVIDER.TEST"},
		{"multica host 443", multicaHost, multicaHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := defaultRig(t)
			c := r.dial()
			code, br := status(t, c, connect(tc.authority, "443"))
			if code != 200 {
				t.Fatalf("status %d, want 200", code)
			}
			h := realHello(t, tc.sni)
			c.Write(h)
			echo := make([]byte, len(h))
			if _, err := io.ReadFull(br, echo); err != nil {
				t.Fatalf("tunnel: %v", err)
			}
			if r.sink.received() != len(h) {
				t.Fatalf("remote got %d bytes, want the whole hello (%d)", r.sink.received(), len(h))
			}
		})
	}
}

func TestElevatedMulticaPort(t *testing.T) {
	r := defaultRig(t)
	c := r.dial()
	code, _ := status(t, c, connect(multicaHost, multicaPort))
	if code != 200 {
		t.Fatalf("multica host on its configured port: status %d, want 200", code)
	}
}

func TestElevatedRefusedAuthorities(t *testing.T) {
	cases := map[string]string{
		// T17 (b): GitHub IP literals in every spelling
		"ipv4 literal":              "140.82.112.3:443",
		"ipv6 literal":              "[2606:50c0:8000::154]:443",
		"ipv4-mapped ipv6":          "[::ffff:140.82.112.3]:443",
		"ipv4-mapped hex form":      "[::ffff:8c52:7003]:443",
		"integer ipv4":              "2352877571:443",
		"hex ipv4":                  "0x8c527003:443",
		"dotted hex ipv4":           "0x8c.0x52.0x70.0x03:443",
		"octal ipv4":                "0214.0122.0160.03:443",
		"short ipv4":                "140.5392387:443",
		"literal with trailing dot": "140.82.112.3.:443",
		"bracketed hostname":        "[" + providerHost + "]:443",
		"user-info":                 "user@" + providerHost + ":443",
		"user-info with host":       providerHost + "@evil.test:443",
		"path":                      providerHost + "/x:443",
		"percent encoded":           "api%2eprovider.test:443",
		"two trailing dots":         providerHost + "..:443",
		"leading dot":               "." + providerHost + ":443",
		"empty label":               "api..provider.test:443",
		"subdomain":                 "x." + providerHost + ":443",
		"parent domain":             "provider.test:443",
		// T17 (c): alternate and relay names
		"github api": "API.GITHUB.COM.:443",
		"raw":        "raw.githubusercontent.com:443",
		"objects":    "objects.githubusercontent.com:443",
		"codeload":   "codeload.github.com:443",
		"fastly":     "github.global.ssl.fastly.net:443",
		"jsdelivr":   "cdn.jsdelivr.net:443",
		"goproxy":    "proxy.golang.org:443",
		// ports
		"provider port 80":      providerHost + ":80",
		"provider port 22":      providerHost + ":22",
		"provider multica port": providerHost + ":" + multicaPort,
		"padded port":           providerHost + ":0443",
		"port 4430":             providerHost + ":4430",
		"multica port 22":       multicaHost + ":22",
		"multica other port":    multicaHost + ":9443",
	}
	for name, authority := range cases {
		t.Run(name, func(t *testing.T) {
			r := defaultRig(t)
			code, _ := status(t, r.dial(), rawConnect(authority))
			if code != http.StatusForbidden && code != http.StatusBadRequest {
				t.Fatalf("CONNECT %s: status %d, want 403 or 400", authority, code)
			}
			if r.sink.conns != 0 {
				t.Fatalf("CONNECT %s: proxy dialled upstream", authority)
			}
			if n := r.res.calls.Load(); n != 0 {
				t.Fatalf("CONNECT %s: proxy resolved a refused name (%d lookups)", authority, n)
			}
			if code == http.StatusForbidden {
				d := r.p.Dests()
				if len(d) != 1 || d[0].Allowed || d[0].Reason == "" {
					t.Fatalf("refusal not logged with a reason: %+v", d)
				}
			}
		})
	}
}

// T17 (e): absolute-form requests stay refused under the elevated policy.
func TestElevatedAbsoluteFormRefused(t *testing.T) {
	r := defaultRig(t)
	code, _ := status(t, r.dial(), "GET http://"+providerHost+"/ HTTP/1.1\r\nHost: "+providerHost+"\r\n\r\n")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", code)
	}
	if r.sink.conns != 0 || r.res.calls.Load() != 0 {
		t.Fatal("proxy acted on an absolute-form request")
	}
	if d := r.p.Dests(); len(d) != 1 || d[0].Allowed || d[0].Reason == "" {
		t.Fatalf("refusal not logged with a reason: %+v", d)
	}
}

// Requests refused before the policy runs are still logged (T17: a proxy log
// line wherever the proxy saw the request).
func TestElevatedEarlyRefusalsAreLogged(t *testing.T) {
	cases := map[string]struct {
		req  string
		code int
	}{
		"origin-form GET":  {"GET / HTTP/1.1\r\nHost: " + providerHost + "\r\n\r\n", 405},
		"POST":             {"POST http://" + providerHost + "/ HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", 405},
		"no port":          {"CONNECT " + providerHost + " HTTP/1.1\r\nHost: " + providerHost + "\r\n\r\n", 400},
		"non-numeric port": {rawConnect(providerHost + ":x"), 400},
		"empty host":       {rawConnect(":443"), 400},
		"encoded host":     {rawConnect("api%2eprovider.test:443"), 400},
		"garbage":          {"\x00\x01 not http\r\n\r\n", 400},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := defaultRig(t)
			code, _ := status(t, r.dial(), tc.req)
			if code != tc.code {
				t.Fatalf("status %d, want %d", code, tc.code)
			}
			d := r.p.Dests()
			if len(d) != 1 || d[0].Allowed || d[0].Reason == "" {
				t.Fatalf("refusal not logged with a reason: %+v", d)
			}
			if len(d[0].Host) > maxLoggedTarget {
				t.Fatalf("logged host is %d bytes, cap is %d", len(d[0].Host), maxLoggedTarget)
			}
		})
	}
}

func TestElevatedEarlyRefusalLogIsCapped(t *testing.T) {
	r := defaultRig(t)
	status(t, r.dial(), rawConnect(strings.Repeat("a", 3000)+":x"))
	d := r.p.Dests()
	if len(d) != 1 || len(d[0].Host) > maxLoggedTarget {
		t.Fatalf("destination log = %d entries, host length %d", len(d), len(d[0].Host))
	}
}

// --- resolver and address class ---

func TestElevatedResolvedAddressClasses(t *testing.T) {
	bad := map[string]netip.Addr{
		"loopback":           netip.MustParseAddr("127.0.0.1"),
		"loopback v6":        netip.MustParseAddr("::1"),
		"v4-mapped loopback": netip.MustParseAddr("::ffff:127.0.0.1"),
		"link-local":         netip.MustParseAddr("169.254.169.254"),
		"link-local v6":      netip.MustParseAddr("fe80::1"),
		"unspecified":        netip.MustParseAddr("0.0.0.0"),
		"unspecified v6":     netip.MustParseAddr("::"),
		"multicast":          netip.MustParseAddr("224.0.0.1"),
		"multicast v6":       netip.MustParseAddr("ff02::1"),
		"private 10/8":       netip.MustParseAddr("10.1.2.3"),
		"private 172.16/12":  netip.MustParseAddr("172.16.0.5"),
		"private 192.168/16": netip.MustParseAddr("192.168.1.1"),
		"unique local v6":    netip.MustParseAddr("fd00::1"),
		"v4-mapped private":  netip.MustParseAddr("::ffff:10.0.0.1"),
		"tailnet 100.64/10":  netip.MustParseAddr("100.101.102.103"),
		"broadcast":          netip.MustParseAddr("255.255.255.255"),
	}
	for name, a := range bad {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, map[string][]netip.Addr{providerHost: {a}})
			code, _ := status(t, r.dial(), connect(providerHost, "443"))
			if code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", code)
			}
			if r.sink.conns != 0 {
				t.Fatal("proxy dialled a refused address")
			}
			if d := r.p.Dests(); len(d) != 1 || d[0].Allowed || d[0].Reason == "" {
				t.Fatalf("refusal not logged with a reason: %+v", d)
			}
		})
	}
}

func TestElevatedMixedAnswerRefusedWhole(t *testing.T) {
	r := newRig(t, map[string][]netip.Addr{providerHost: {publicA, netip.MustParseAddr("10.0.0.5")}})
	code, _ := status(t, r.dial(), connect(providerHost, "443"))
	if code != http.StatusForbidden || r.sink.conns != 0 {
		t.Fatalf("status %d, conns %d: a private answer must refuse the name", code, r.sink.conns)
	}
}

func TestElevatedPinnedAddressOnlyForMulticaHost(t *testing.T) {
	loop := netip.MustParseAddr("127.0.0.1")
	// pinned loopback is fine for the Multica host ...
	r := newRig(t, map[string][]netip.Addr{multicaHost: {loop}, providerHost: {loop}}, loop)
	if code, _ := status(t, r.dial(), connect(multicaHost, "443")); code != 200 {
		t.Fatalf("multica host at its pinned address: status %d, want 200", code)
	}
	// ... but the provider host resolving to it (rebinding) is not.
	if code, _ := status(t, r.dial(), connect(providerHost, "443")); code != http.StatusForbidden {
		t.Fatalf("provider host at the pinned address: status %d, want 403", code)
	}
	// an unpinned loopback answer for the Multica host is refused.
	r = newRig(t, map[string][]netip.Addr{multicaHost: {loop}})
	if code, _ := status(t, r.dial(), connect(multicaHost, "443")); code != http.StatusForbidden {
		t.Fatalf("multica host at an unpinned loopback address: status %d, want 403", code)
	}
}

func TestElevatedResolverFailureAndEmptyAnswer(t *testing.T) {
	r := defaultRig(t)
	r.res.err = errors.New("nxdomain")
	if code, _ := status(t, r.dial(), connect(providerHost, "443")); code != http.StatusBadGateway {
		t.Fatalf("resolver error: status %d, want 502", code)
	}
	r = newRig(t, map[string][]netip.Addr{})
	if code, _ := status(t, r.dial(), connect(providerHost, "443")); code != http.StatusBadGateway {
		t.Fatalf("empty answer: status %d, want 502", code)
	}
}

func TestElevatedDialsOnlyTheCheckedAddress(t *testing.T) {
	r := defaultRig(t)
	status(t, r.dial(), connect(providerHost, "443"))
	if len(r.sink.dials) != 1 || r.sink.dials[0] != "203.0.113.10:443" {
		t.Fatalf("dialled %v, want only the resolved address 203.0.113.10:443", r.sink.dials)
	}
	if n := r.res.calls.Load(); n != 1 {
		t.Fatalf("%d lookups, want 1 (no second resolution at dial time)", n)
	}
}

// --- T17 (d): SNI match ---

func TestElevatedSNIMismatchRefused(t *testing.T) {
	r := defaultRig(t)
	expectHelloRefused(t, r, providerHost, realHello(t, "api.github.com"))
}

func TestElevatedSNIRefusals(t *testing.T) {
	cases := map[string][]byte{
		"no sni extension":      hello(ext{10, []byte{0, 2, 0, 29}}),
		"no extensions":         hello(),
		"empty name":            hello(sniExt("")),
		"ip literal sni":        hello(sniExt("203.0.113.10")),
		"two names":             hello(sniExt(providerHost, providerHost)),
		"duplicate sni ext":     hello(sniExt(providerHost), sniExt(providerHost)),
		"ech present":           hello(sniExt(providerHost), ext{0xfe0d, []byte{0, 0}}),
		"legacy esni present":   hello(sniExt(providerHost), ext{0xffce, []byte{0, 0}}),
		"sni with trailing nul": hello(sniExt(providerHost + "\x00.github.com")),
		"not tls":               []byte("GET / HTTP/1.1\r\nHost: " + providerHost + "\r\n\r\n"),
		"truncated record":      hello(sniExt(providerHost))[:20],
		"not a client hello":    {0x17, 3, 3, 0, 1, 0},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			r := defaultRig(t)
			c := r.dial()
			if code, _ := status(t, c, connect(providerHost, "443")); code != 200 {
				t.Fatalf("CONNECT: status %d", code)
			}
			c.Write(h)
			if cw, ok := c.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite() // lets a truncated hello end instead of waiting for the timeout
			}
			buf := make([]byte, 1)
			if n, _ := c.Read(buf); n != 0 {
				t.Fatal("proxy answered instead of closing")
			}
			if n := r.sink.received(); n != 0 {
				t.Fatalf("%d bytes reached the remote host", n)
			}
			d := r.p.Dests()
			if last := d[len(d)-1]; last.Allowed || last.Reason == "" {
				t.Fatalf("refusal not logged with a reason: %+v", last)
			}
		})
	}
}

func TestElevatedHelloSplitAcrossRecordsRefused(t *testing.T) {
	h := hello(sniExt(providerHost))
	// cut the handshake message in two records; the first alone is not a full hello
	first := append([]byte{0x16, 3, 1}, append(u16(10), h[5:15]...)...)
	second := append([]byte{0x16, 3, 1}, append(u16(len(h)-15), h[15:]...)...)
	r := defaultRig(t)
	expectHelloRefused(t, r, providerHost, append(first, second...))
}

func TestElevatedSilentClientTimesOut(t *testing.T) {
	r := defaultRig(t)
	r.p.Elevated.helloTimeout = 100 * time.Millisecond
	c := r.dial()
	status(t, c, connect(providerHost, "443"))
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("expected the proxy to hang up on a client that sends no hello")
	}
	if r.sink.received() != 0 {
		t.Fatal("bytes reached the remote host")
	}
}

// --- policy construction ---

func TestNewElevatedPolicyValidation(t *testing.T) {
	ok := func(mh string, mp, ph []string) error {
		_, err := NewElevatedPolicy(mh, mp, ph, nil, &fakeResolver{})
		return err
	}
	if err := ok(multicaHost, []string{"443", "8443"}, []string{providerHost}); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	if err := ok(multicaHost, []string{"443"}, []string{providerHost, "API.Provider.Test"}); err != nil {
		t.Fatalf("one provider host named twice: %v", err)
	}
	for name, err := range map[string]error{
		"ip multica host":   ok("100.64.0.1", []string{"443"}, []string{providerHost}),
		"ip provider host":  ok(multicaHost, []string{"443"}, []string{"203.0.113.10"}),
		"bracketed host":    ok(multicaHost, []string{"443"}, []string{"[::1]"}),
		"empty host":        ok("", []string{"443"}, []string{providerHost}),
		"empty provider":    ok(multicaHost, []string{"443"}, []string{""}),
		"no provider":       ok(multicaHost, []string{"443"}, nil),
		"two providers":     ok(multicaHost, []string{"443"}, []string{providerHost, "other.provider.test"}),
		"wildcard":          ok(multicaHost, []string{"443"}, []string{"*.provider.test"}),
		"host with port":    ok(multicaHost, []string{"443"}, []string{providerHost + ":443"}),
		"bad multica port":  ok(multicaHost, []string{"https"}, []string{providerHost}),
		"padded port":       ok(multicaHost, []string{"0443"}, []string{providerHost}),
		"ssh port":          ok(multicaHost, []string{"22"}, []string{providerHost}),
		"port zero":         ok(multicaHost, []string{"0"}, []string{providerHost}),
		"port out of range": ok(multicaHost, []string{"70000"}, []string{providerHost}),
	} {
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := NewElevatedPolicy(multicaHost, []string{"443"}, []string{providerHost}, nil, nil); err != nil {
		t.Fatalf("nil resolver should default to the system resolver: %v", err)
	}
}

// The restricted policy is untouched by the elevated one.
func TestRestrictedPolicyUnchangedWhenElevatedNil(t *testing.T) {
	port := origin(t)
	dial := startProxy(t, &Proxy{AllowLoopback: true})
	code, _ := status(t, dial(), connect("127.0.0.1", port))
	if code != 200 {
		t.Fatalf("restricted CONNECT: status %d, want 200", code)
	}
}
