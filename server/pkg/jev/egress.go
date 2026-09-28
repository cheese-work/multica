package jev

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// EgressAllowlistEnv names the comma-separated exact host:port allowlist.
	EgressAllowlistEnv = "MULTICA_JEV_EGRESS_ALLOWLIST"
	maxResponseBytes   = 1 << 20
	maxRequestTimeout  = 30 * time.Second
)

var (
	// ErrEgressDenied reports a destination rejected before dialing.
	ErrEgressDenied = errors.New("jev: egress policy denied")
	// ErrResponseTooLarge reports an upstream body exceeding the read limit.
	ErrResponseTooLarge = errors.New("jev: upstream response exceeds size limit")
	// ErrUpstreamRequest is a scrubbed transport failure.
	ErrUpstreamRequest = errors.New("jev: upstream request failed")
)

type ipLookupFunc func(context.Context, string) ([]net.IP, error)
type dialContextFunc func(context.Context, string, string) (net.Conn, error)

var blockedAddressPrefixes = mustAddressPrefixes(
	"0.0.0.0/8",
	"100.64.0.0/10",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.88.99.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"240.0.0.0/4",
	"64:ff9b::/96",
	"100::/64",
	"2001::/23",
	"2001:db8::/32",
	"2002::/16",
	"3fff::/20",
)

func mustAddressPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			panic("invalid blocked address prefix")
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

func normalizeBaseURL(raw string) (string, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("jev: invalid BaseURL")
	}

	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	authority, err := normalizeHostPort(parsed.Hostname(), port)
	if err != nil {
		return "", "", errors.New("jev: invalid BaseURL")
	}
	parsed.Scheme = "https"
	parsed.Host = authority
	return strings.TrimRight(parsed.String(), "/"), authority, nil
}

func loadEgressAllowlist(environment string, additional []string) (map[string]struct{}, error) {
	entries := make([]string, 0, len(additional)+1)
	if strings.TrimSpace(environment) != "" {
		entries = append(entries, strings.Split(environment, ",")...)
	}
	entries = append(entries, additional...)

	allowed := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		authority, err := normalizeAllowlistEntry(entry)
		if err != nil {
			return nil, errors.New("jev: invalid egress allowlist")
		}
		allowed[authority] = struct{}{}
	}
	return allowed, nil
}

func normalizeAllowlistEntry(entry string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(entry))
	if err != nil {
		return "", err
	}
	return normalizeHostPort(host, port)
}

func normalizeHostPort(host, port string) (string, error) {
	if host == "" || strings.Contains(host, "%") || port == "" {
		return "", errors.New("invalid host or port")
	}
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return "", errors.New("invalid port")
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", errors.New("invalid port")
	}

	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else if !validDNSName(host) {
		return "", errors.New("invalid hostname")
	}
	return net.JoinHostPort(host, strconv.Itoa(portNumber)), nil
}

func validDNSName(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func newEgressHTTPClient(base *http.Client, allowed map[string]struct{}, timeout time.Duration, lookup ipLookupFunc, dial dialContextFunc) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if base != nil && base.Transport != nil {
		provided, ok := base.Transport.(*http.Transport)
		if !ok {
			return nil, errors.New("jev: unsupported HTTP transport")
		}
		transport = provided.Clone()
	}
	transport.Proxy = nil
	transport.DialTLS = nil
	transport.DialTLSContext = nil
	transport.TLSNextProto = nil
	transport.DialContext = newPinnedDialContext(allowed, lookup, dial)
	if transport.MaxResponseHeaderBytes == 0 || transport.MaxResponseHeaderBytes > 64<<10 {
		transport.MaxResponseHeaderBytes = 64 << 10
	}
	if transport.TLSClientConfig != nil {
		tlsConfig := transport.TLSClientConfig.Clone()
		tlsConfig.ServerName = ""
		tlsConfig.InsecureSkipVerify = false
		transport.TLSClientConfig = tlsConfig
	}

	client := &http.Client{Transport: egressRoundTripper{transport: transport, allowed: allowed}, Timeout: timeout}
	if base != nil {
		client.Jar = base.Jar
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client, nil
}

type egressRoundTripper struct {
	transport *http.Transport
	allowed   map[string]struct{}
}

func (r egressRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.URL.User != nil || !strings.EqualFold(request.URL.Scheme, "https") {
		return nil, ErrEgressDenied
	}
	port := request.URL.Port()
	if port == "" {
		port = "443"
	}
	authority, err := normalizeHostPort(request.URL.Hostname(), port)
	if err != nil {
		return nil, ErrEgressDenied
	}
	if _, ok := r.allowed[authority]; !ok || isMetadataHost(strings.Trim(request.URL.Hostname(), "[]")) {
		return nil, ErrEgressDenied
	}
	response, err := r.transport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxResponseBytes {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, ErrResponseTooLarge
	}
	if response.Body != nil {
		response.Body = &boundedReadCloser{body: response.Body, remaining: maxResponseBytes}
	}
	return response, nil
}

type boundedReadCloser struct {
	body      io.ReadCloser
	remaining int64
}

func (r *boundedReadCloser) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var probe [1]byte
		count, err := r.body.Read(probe[:])
		if count > 0 {
			return 0, ErrResponseTooLarge
		}
		return 0, err
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	count, err := r.body.Read(buffer)
	r.remaining -= int64(count)
	return count, err
}

func (r *boundedReadCloser) Close() error { return r.body.Close() }

func newPinnedDialContext(allowed map[string]struct{}, lookup ipLookupFunc, dial dialContextFunc) func(context.Context, string, string) (net.Conn, error) {
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	if dial == nil {
		dialer := &net.Dialer{}
		dial = dialer.DialContext
	}

	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, ErrEgressDenied
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrEgressDenied
		}
		authority, err := normalizeHostPort(host, port)
		if err != nil {
			return nil, ErrEgressDenied
		}
		if _, ok := allowed[authority]; !ok || isMetadataHost(strings.Trim(host, "[]")) {
			return nil, ErrEgressDenied
		}

		normalizedHost := strings.Trim(strings.ToLower(host), "[]")
		literalIP := net.ParseIP(normalizedHost)
		addresses := []net.IP{literalIP}
		if literalIP == nil {
			addresses, err = lookup(ctx, normalizedHost)
			if err != nil || len(addresses) == 0 {
				return nil, ErrUpstreamRequest
			}
		}
		for _, ip := range addresses {
			if ip == nil || forbiddenAddress(ip, literalIP != nil) {
				return nil, ErrEgressDenied
			}
		}

		for _, ip := range addresses {
			connection, dialErr := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
		}
		return nil, ErrUpstreamRequest
	}
}

func forbiddenAddress(ip net.IP, literal bool) bool {
	parsed, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	parsed = parsed.Unmap()
	if isMetadataIP(parsed) {
		return true
	}
	if parsed.IsLoopback() || parsed.IsPrivate() {
		return !literal
	}
	if parsed.IsUnspecified() || parsed.IsMulticast() || parsed.IsLinkLocalUnicast() || parsed.IsLinkLocalMulticast() || !parsed.IsGlobalUnicast() {
		return true
	}
	for _, prefix := range blockedAddressPrefixes {
		if prefix.Contains(parsed) {
			return true
		}
	}
	return false
}

func isMetadataIP(address netip.Addr) bool {
	return address == netip.MustParseAddr("169.254.169.254") || address == netip.MustParseAddr("fd00:ec2::254") || address == netip.MustParseAddr("100.100.100.200")
}

func isMetadataHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip := net.ParseIP(host); ip != nil {
		address, ok := netip.AddrFromSlice(ip)
		return !ok || isMetadataIP(address.Unmap())
	}
	switch host {
	case "metadata", "metadata.google.internal", "metadata.goog", "instance-data":
		return true
	default:
		return false
	}
}
