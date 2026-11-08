package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProviderAlertSink delivers one alert. key identifies the incident; the
// monitor never sends the same key twice after a successful delivery.
type ProviderAlertSink interface {
	Alert(ctx context.Context, key, message string) error
}

// ProviderProbeMonitor probes a provider gateway on a schedule (CHE-2804).
// A transport error or 5xx opens an incident; any other status is healthy
// (401 counts: the gateway answered). Silence is itself an incident: no probe
// result within staleAfter raises a separate heartbeat alert.
type ProviderProbeMonitor struct {
	provider   string
	probe      ProviderFailureProbe
	sink       ProviderAlertSink
	now        func() time.Time
	window     time.Duration
	staleAfter time.Duration

	mu         sync.Mutex
	lastResult time.Time
	failStart  time.Time // zero when the last result was healthy
	sent       map[string]struct{}
}

// NewProviderProbeMonitor: window buckets the failure start into the incident
// key; staleAfter is the heartbeat interval.
func NewProviderProbeMonitor(provider string, probe ProviderFailureProbe, sink ProviderAlertSink, now func() time.Time, window, staleAfter time.Duration) *ProviderProbeMonitor {
	return &ProviderProbeMonitor{
		provider: provider, probe: probe, sink: sink, now: now,
		window: window, staleAfter: staleAfter,
		lastResult: now(), sent: map[string]struct{}{},
	}
}

// Run ticks every staleAfter/2 until ctx ends.
func (m *ProviderProbeMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.staleAfter / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Tick(ctx)
		}
	}
}

// Tick probes once, records the outcome, alerts on a new incident, then runs
// the heartbeat check.
func (m *ProviderProbeMonitor) Tick(ctx context.Context) {
	evidence, err := m.probe.Probe(ctx)
	now := m.now()
	m.mu.Lock()
	if err == nil {
		m.lastResult = now
	}
	failed := err != nil || evidence.StatusCode >= http.StatusInternalServerError
	if !failed {
		m.failStart = time.Time{}
	} else if m.failStart.IsZero() {
		m.failStart = now
	}
	failStart := m.failStart
	m.mu.Unlock()
	if failed {
		key := fmt.Sprintf("%s:failure:%d", m.provider, failStart.Truncate(m.window).Unix())
		m.alertOnce(ctx, key, fmt.Sprintf("provider %s probe failing since %s (status=%d err=%v)", m.provider, failStart.Format(time.RFC3339), evidence.StatusCode, err))
	}
	m.CheckHeartbeat(ctx)
}

// CheckHeartbeat alerts when no probe result was recorded within staleAfter.
func (m *ProviderProbeMonitor) CheckHeartbeat(ctx context.Context) {
	m.mu.Lock()
	last := m.lastResult
	m.mu.Unlock()
	if m.now().Sub(last) <= m.staleAfter {
		return
	}
	key := fmt.Sprintf("%s:heartbeat:%d", m.provider, last.Truncate(m.window).Unix())
	m.alertOnce(ctx, key, fmt.Sprintf("provider %s probe silent since %s", m.provider, last.Format(time.RFC3339)))
}

func (m *ProviderProbeMonitor) alertOnce(ctx context.Context, key, message string) {
	m.mu.Lock()
	_, done := m.sent[key]
	m.mu.Unlock()
	if done {
		return
	}
	if err := m.sink.Alert(ctx, key, message); err != nil {
		slog.Warn("provider probe alert failed", "key", key, "error", err)
		return // not marked sent: retried on the next tick
	}
	m.mu.Lock()
	m.sent[key] = struct{}{}
	m.mu.Unlock()
}

// WithBearerToken returns a copy of the probe that sends token on every
// request. The token must be a read-only credential (GET /v1/models only).
func (p *HTTPProviderFailureProbe) WithBearerToken(token string) *HTTPProviderFailureProbe {
	cp := *p
	cp.token = strings.TrimSpace(token)
	return &cp
}

// LoadProviderProbeCredential reads the probe credential from env, else from
// an absolute file path. Relative paths are refused so the secret cannot live
// in a repo-tracked file. Both empty means no credential.
var errBearerOverPlainHTTP = errors.New("refusing to send a bearer token over plain HTTP to a non-loopback host")

func LoadProviderProbeCredential(env, file string) (string, error) {
	if env = strings.TrimSpace(env); env != "" {
		return env, nil
	}
	if file = strings.TrimSpace(file); file == "" {
		return "", nil
	}
	if !filepath.IsAbs(file) {
		return "", fmt.Errorf("provider probe credential file must be an absolute path outside the repo")
	}
	real, err := filepath.EvalSymlinks(file)
	if err != nil {
		return "", fmt.Errorf("resolve provider probe credential file: %w", err)
	}
	for dir := filepath.Dir(real); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return "", fmt.Errorf("provider probe credential file is inside a git work tree")
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		return "", fmt.Errorf("read provider probe credential file: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// tokenSafeURL: a bearer token goes only over https or to a loopback host.
func tokenSafeURL(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
