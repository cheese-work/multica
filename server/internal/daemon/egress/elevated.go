package egress

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxRecord      = 1 << 14 // TLS plaintext record limit
	extServerName  = 0
	extECH         = 0xfe0d
	extLegacyESNI  = 0xffce
	httpsPort      = "443"
	sshPort        = 22
	recordHandshke = 0x16
)

// Address classes the proxy never dials, beyond what netip classifies:
// "this network", the tailnet CGNAT range and the reserved block (which holds
// the IPv4 broadcast address).
var specialUse = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// Resolver looks a name up. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ElevatedPolicy is the egress policy of an elevated run (plan 3.3): default
// deny; exact hostnames only; CONNECT to a hostname, never an address; the
// proxy resolves and dials the checked address itself; the TLS SNI must equal
// the CONNECT host.
type ElevatedPolicy struct {
	hosts       map[string]map[string]bool // normalized host -> allowed ports
	multicaHost string
	pinned      []netip.Addr // the Multica server's own addresses, allowed even if private
	resolver    Resolver
	dial        func(ctx context.Context, network, address string) (net.Conn, error)
	// helloTimeout bounds the wait for the client's ClientHello after the
	// CONNECT is accepted.
	helloTimeout time.Duration
}

// NewElevatedPolicy allows multicaHost on 443 and multicaPorts, and each of
// providerHosts on 443. Every entry must be a DNS hostname. pinned lists the
// addresses multicaHost may resolve to even when they are private. A nil
// resolver uses the system resolver.
func NewElevatedPolicy(multicaHost string, multicaPorts, providerHosts []string, pinned []netip.Addr, r Resolver) (*ElevatedPolicy, error) {
	if r == nil {
		r = net.DefaultResolver
	}
	e := &ElevatedPolicy{hosts: map[string]map[string]bool{}, resolver: r, helloTimeout: headerTimeout}
	for _, a := range pinned {
		e.pinned = append(e.pinned, a.Unmap())
	}
	d := net.Dialer{Timeout: dialTimeout}
	e.dial = d.DialContext

	mh, err := allowlistHost(multicaHost)
	if err != nil {
		return nil, fmt.Errorf("egress: multica host: %w", err)
	}
	e.multicaHost = mh
	e.allow(mh, httpsPort)
	for _, p := range multicaPorts {
		if err := checkPort(p); err != nil {
			return nil, fmt.Errorf("egress: multica port: %w", err)
		}
		e.allow(mh, p)
	}
	// Plan 3.3: the Multica host and the runtime's one provider host, nothing else.
	provider := ""
	for _, h := range providerHosts {
		ph, err := allowlistHost(h)
		if err != nil {
			return nil, fmt.Errorf("egress: provider host: %w", err)
		}
		if provider != "" && ph != provider {
			return nil, fmt.Errorf("egress: more than one provider host (%q, %q)", provider, ph)
		}
		provider = ph
	}
	if provider == "" {
		return nil, errors.New("egress: no provider host")
	}
	e.allow(provider, httpsPort)
	return e, nil
}

func (e *ElevatedPolicy) allow(host, port string) {
	if e.hosts[host] == nil {
		e.hosts[host] = map[string]bool{}
	}
	e.hosts[host][port] = true
}

func allowlistHost(h string) (string, error) {
	h = strings.ToLower(h)
	if reason := badHostname(h); reason != "" {
		return "", fmt.Errorf("%q: %s", h, reason)
	}
	return h, nil
}

func checkPort(p string) error {
	n, err := strconv.Atoi(p)
	if err != nil || strconv.Itoa(n) != p || n < 1 || n > 65535 || n == sshPort {
		return fmt.Errorf("invalid port %q", p)
	}
	return nil
}

// badHostname returns why s (already lower-case, no trailing dot) is not a
// plain DNS hostname, or "". Rejecting by shape covers every IP spelling:
// bracketed and mapped IPv6 (':' and '[' are outside the alphabet), and
// integer, octal, hex and short IPv4 (a numeric last label, as a URL parser
// reads it).
func badHostname(s string) string {
	if s == "" {
		return "empty host"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return "not a plain DNS hostname"
		}
	}
	labels := strings.Split(s, ".")
	if slices.Contains(labels, "") {
		return "empty label"
	}
	last := labels[len(labels)-1]
	if isDigits(last) || strings.HasPrefix(last, "0x") && isHex(last[2:]) {
		return "numeric last label (IP address spelling)"
	}
	return ""
}

func isHex(s string) bool {
	_, err := strconv.ParseUint(s, 16, 64)
	return s == "" || err == nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// hostLocal reports an address the proxy must not dial: it runs on the host,
// so these reach host or internal services the run's netns hides.
func hostLocal(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() || a.IsPrivate() {
		return true
	}
	return slices.ContainsFunc(specialUse, func(p netip.Prefix) bool { return p.Contains(a) })
}

// handleElevated serves one CONNECT under the elevated policy. authority is
// target is the request target as sent; parsed is what net/http made of it.
func (p *Proxy) handleElevated(c net.Conn, br *bufio.Reader, target, parsed, host, port string) {
	e := p.Elevated
	refuse := func(reason string) {
		p.record(host, port, false, reason)
		reply(c, http.StatusForbidden)
	}
	if target != parsed { // net/http drops user-info, for one
		refuse("authority is not a bare host:port")
		return
	}
	if strings.HasPrefix(target, "[") {
		refuse("bracketed authority")
		return
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if reason := badHostname(host); reason != "" {
		refuse(reason)
		return
	}
	ports, ok := e.hosts[host]
	if !ok {
		refuse("host not on the allowlist")
		return
	}
	if !ports[port] {
		refuse("port not allowed for this host")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	addrs, err := e.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		p.record(host, port, false, "resolution failed")
		reply(c, http.StatusBadGateway)
		return
	}
	for _, a := range addrs {
		if hostLocal(a) && !(host == e.multicaHost && slices.Contains(e.pinned, a.Unmap())) {
			refuse("resolved address is host-local, private or special-use")
			return
		}
	}

	i := p.record(host, port, true, "")
	var up net.Conn
	for _, a := range addrs {
		if up, err = e.dial(ctx, "tcp", net.JoinHostPort(a.Unmap().String(), port)); err == nil {
			break
		}
	}
	if up == nil {
		p.refuse(i, "dial failed")
		reply(c, http.StatusBadGateway)
		return
	}
	defer up.Close()

	fmt.Fprint(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = c.SetReadDeadline(time.Now().Add(e.helloTimeout))
	rec, sni, err := readClientHello(br)
	if err == nil && sni != host {
		err = fmt.Errorf("SNI %q does not match CONNECT host", sni)
	}
	if err != nil {
		p.refuse(i, "tls: "+err.Error())
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if _, err := up.Write(rec); err != nil {
		return
	}
	tunnel(c, br, up)
}

// readClientHello reads the first TLS record, which must hold a complete
// ClientHello, and returns the record bytes and its SNI (normalized). It
// refuses a hello with no SNI, more than one name, Encrypted ClientHello or
// duplicate extensions, since each leaves the SNI check ambiguous.
func readClientHello(r io.Reader) (record []byte, sni string, err error) {
	hdr := make([]byte, 5)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return nil, "", fmt.Errorf("read hello: %w", err)
	}
	if hdr[0] != recordHandshke || hdr[1] != 3 {
		return nil, "", errors.New("first record is not a TLS handshake")
	}
	n := int(binary.BigEndian.Uint16(hdr[3:]))
	if n == 0 || n > maxRecord {
		return nil, "", errors.New("bad record length")
	}
	body := make([]byte, n)
	if _, err = io.ReadFull(r, body); err != nil {
		return nil, "", fmt.Errorf("read hello: %w", err)
	}
	sni, err = parseSNI(body)
	if err != nil {
		return nil, "", err
	}
	return append(hdr, body...), sni, nil
}

// reader walks a length-prefixed buffer; any overrun sets bad.
type reader struct {
	b   []byte
	bad bool
}

func (r *reader) take(n int) []byte {
	if n < 0 || n > len(r.b) {
		r.bad = true
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) u8() int {
	if v := r.take(1); v != nil {
		return int(v[0])
	}
	return 0
}

func (r *reader) u16() int {
	if v := r.take(2); v != nil {
		return int(binary.BigEndian.Uint16(v))
	}
	return 0
}

func parseSNI(hs []byte) (string, error) {
	r := &reader{b: hs}
	if r.u8() != 1 { // client_hello
		return "", errors.New("not a ClientHello")
	}
	l := r.take(3)
	if len(l) != 3 || int(l[0])<<16|int(l[1])<<8|int(l[2]) != len(r.b) {
		return "", errors.New("ClientHello does not fill its record")
	}
	r.take(2 + 32)  // version, random
	r.take(r.u8())  // session id
	r.take(r.u16()) // cipher suites
	r.take(r.u8())  // compression methods
	exts := &reader{b: r.take(r.u16())}
	if r.bad || len(r.b) != 0 {
		return "", errors.New("malformed ClientHello")
	}
	var names []byte
	seen := map[int]bool{}
	for len(exts.b) > 0 && !exts.bad {
		typ := exts.u16()
		body := exts.take(exts.u16())
		if exts.bad {
			break
		}
		if seen[typ] {
			return "", errors.New("duplicate extension")
		}
		seen[typ] = true
		if typ == extServerName {
			names = body
		}
	}
	if exts.bad {
		return "", errors.New("malformed extensions")
	}
	if seen[extECH] || seen[extLegacyESNI] {
		return "", errors.New("encrypted ClientHello present")
	}
	if !seen[extServerName] {
		return "", errors.New("no SNI")
	}
	list := &reader{b: names}
	list = &reader{b: list.take(list.u16())}
	if list.bad || len(names) != 2+len(list.b) {
		return "", errors.New("malformed server_name")
	}
	if list.u8() != 0 { // host_name
		return "", errors.New("SNI is not a host_name")
	}
	name := list.take(list.u16())
	if list.bad || len(list.b) != 0 {
		return "", errors.New("SNI must hold exactly one name")
	}
	return strings.TrimSuffix(strings.ToLower(string(name)), "."), nil
}
