// Package egress is the host side of the run's only network path (CHE-2216
// slice A7b). A sandboxed run reaches it through the in-netns forwarder
// (sandbox/bridge.go) over a Unix socket. The proxy accepts CONNECT only,
// applies the egress policy, logs every destination and marks the run
// external_read when it contacts a host other than the Multica server or the
// provider API (plan 3.7).
package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	headerTimeout = 10 * time.Second
	dialTimeout   = 10 * time.Second
)

// Dest is one CONNECT target the run asked for.
type Dest struct {
	Host, Port string
	Allowed    bool // the policy let it through
	External   bool // not the Multica server or provider API
}

// Proxy serves one run. A nil Allow is the restricted policy: every hostname
// is allowed until CHE-2224 sets an allowlist.
type Proxy struct {
	Allow   func(host, port string) bool
	Trusted []string // hosts that do not mark external_read: Multica server, provider API
	// AllowLoopback lets the proxy dial loopback, link-local and unspecified
	// addresses. Off by default: the proxy runs on the host, so these would
	// reach host services that the run's netns hides.
	AllowLoopback bool
	// OnExternalRead fires once, on the first external destination.
	OnExternalRead func(host string)

	mu       sync.Mutex
	dests    []Dest
	external bool
}

// Dests returns a copy of the destination log, oldest first.
func (p *Proxy) Dests() []Dest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Dest(nil), p.dests...)
}

// ExternalRead reports whether the run contacted an external destination.
func (p *Proxy) ExternalRead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.external
}

// Serve handles connections from ln until it is closed.
func (p *Proxy) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go p.handle(c)
	}
}

func (p *Proxy) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(headerTimeout))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		reply(c, http.StatusBadRequest)
		return
	}
	// CONNECT only: absolute-form and origin-form requests get 405.
	if req.Method != http.MethodConnect {
		reply(c, http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(req.Host)
	if err != nil || host == "" {
		reply(c, http.StatusBadRequest)
		return
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		reply(c, http.StatusBadRequest)
		return
	}
	allowed := port != "22" && (p.Allow == nil || p.Allow(host, port)) // SSH has no path
	p.record(host, port, allowed)
	if !allowed {
		reply(c, http.StatusForbidden)
		return
	}
	d := net.Dialer{Timeout: dialTimeout, Control: p.control}
	up, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		reply(c, http.StatusBadGateway)
		return
	}
	defer up.Close()
	_ = c.SetReadDeadline(time.Time{})
	fmt.Fprint(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	tunnel(c, br, up)
}

// control runs after name resolution, on the address about to be dialled.
func (p *Proxy) control(_, address string, _ syscall.RawConn) error {
	if p.AllowLoopback {
		return nil
	}
	host, _, _ := net.SplitHostPort(address)
	if ip := net.ParseIP(host); ip != nil &&
		(ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
		return fmt.Errorf("egress: %s is a host-local address", ip)
	}
	return nil
}

func (p *Proxy) record(host, port string, allowed bool) {
	external := !p.trusted(host)
	p.mu.Lock()
	p.dests = append(p.dests, Dest{Host: host, Port: port, Allowed: allowed, External: external})
	first := external && allowed && !p.external
	if first {
		p.external = true
	}
	p.mu.Unlock()
	if first && p.OnExternalRead != nil {
		p.OnExternalRead(host)
	}
}

func (p *Proxy) trusted(host string) bool {
	for _, t := range p.Trusted {
		if strings.EqualFold(t, host) {
			return true
		}
	}
	return false
}

func reply(c net.Conn, code int) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nConnection: close\r\n\r\n", code, http.StatusText(code))
}

// tunnel joins client (with bytes already buffered in br) to up until both
// directions end.
func tunnel(client net.Conn, br *bufio.Reader, up net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst net.Conn, src io.Reader) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	go pipe(up, br)
	go pipe(client, up)
	wg.Wait()
}
