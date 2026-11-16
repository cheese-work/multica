// Package controlplane is the second, internal HTTP listener (CHE-4746, W3-17).
//
// The steel-gateway-facing routes (instance event stream, introspection, JWKS,
// cleanup-ack, lease summary) are served here and nowhere else. The public and
// tailnet listener is one router; it never mounts anything under Prefix, so a
// request for these paths there returns 404.
package controlplane

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// Prefix is the path prefix of every gateway-facing route. The public router
// must mount nothing under it (pinned by cmd/server/internal_listener_test.go).
const Prefix = "/internal/browser"

// NewServer builds the internal listener. An empty addr disables it (nil, nil).
//
// addr must be an explicit host:port: the host is an address on the control
// network, never a wildcard, so the listener cannot be reached from any other
// network the host is attached to. The port must differ from publicPort.
// Each mount registers routes relative to Prefix.
func NewServer(addr, publicPort string, mounts ...func(chi.Router)) (*http.Server, error) {
	if addr == "" {
		return nil, nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("internal listen address must be host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if host == "" || (ip != nil && ip.IsUnspecified()) {
		return nil, fmt.Errorf("internal listen address must name one control-network address, not a wildcard")
	}
	if ip == nil && host != "localhost" {
		// A hostname could resolve to a wildcard or several addresses; bind an address.
		return nil, fmt.Errorf("internal listen host must be an IP address")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("internal listen port must be 1..65535")
	}
	if port == publicPort {
		return nil, fmt.Errorf("internal listen port must differ from the public port")
	}

	r := chi.NewRouter()
	r.Route(Prefix, func(r chi.Router) {
		for _, mount := range mounts {
			mount(r)
		}
	})
	// ReadTimeout/WriteTimeout stay zero: the instance event stream is long-lived.
	return &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}, nil
}
