package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// result within staleAfter raises a separate heartbeat alert. The probe loop
// and the heartbeat watchdog run on separate goroutines, so a stalled probe or
// alert sink cannot hide silence. Process death is covered by the optional
// external heartbeat (WithExternalHeartbeat): an outside observer alerts when
// the pings stop.
type ProviderProbeMonitor struct {
	provider     string
	probe        ProviderFailureProbe
	sink         ProviderAlertSink
	now          func() time.Time
	window       time.Duration
	staleAfter   time.Duration
	alertTimeout time.Duration
	beat         *http.Request // external dead-man's-switch GET, nil when unset

	mu         sync.Mutex
	lastResult time.Time
	failStart  time.Time // zero when the last result was healthy
	episode    int       // failure episodes opened so far; part of the incident key
	sent       map[string]struct{}
}

// NewProviderProbeMonitor: window buckets the failure start into the incident
// key; staleAfter is the heartbeat interval.
func NewProviderProbeMonitor(provider string, probe ProviderFailureProbe, sink ProviderAlertSink, now func() time.Time, window, staleAfter time.Duration) *ProviderProbeMonitor {
	return &ProviderProbeMonitor{
		provider: provider, probe: probe, sink: sink, now: now,
		window: window, staleAfter: staleAfter, alertTimeout: providerAlertTimeout,
		lastResult: now(), sent: map[string]struct{}{},
	}
}

const providerAlertTimeout = 5 * time.Second

// WithExternalHeartbeat pings rawURL (GET) after every recorded probe result.
// Point it at a dead-man's-switch service: it alerts when pings stop, which
// the in-process watchdog cannot do if the process dies.
func (m *ProviderProbeMonitor) WithExternalHeartbeat(rawURL string) error {
	u, err := safeAlertURL(rawURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	m.beat = req
	return nil
}

// Run starts the watchdog on its own goroutine and probes every staleAfter/2
// until ctx ends.
func (m *ProviderProbeMonitor) Run(ctx context.Context) {
	go m.watchdog(ctx)
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

func (m *ProviderProbeMonitor) watchdog(ctx context.Context) {
	ticker := time.NewTicker(m.staleAfter / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.CheckHeartbeat(ctx)
		}
	}
}

// Tick probes once, records the outcome, pings the external heartbeat and
// alerts on a new incident.
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
		m.episode++
	}
	failStart, episode := m.failStart, m.episode
	m.mu.Unlock()
	if err == nil {
		m.pingExternalHeartbeat(ctx)
	}
	if failed {
		// The episode number keeps a failure that follows a recovery inside the
		// same window from reusing the earlier incident's key.
		key := fmt.Sprintf("%s:failure:%d:%d", m.provider, failStart.Truncate(m.window).Unix(), episode)
		m.alertOnce(ctx, key, fmt.Sprintf("provider %s probe failing since %s (status=%d err=%v)", m.provider, failStart.Format(time.RFC3339), evidence.StatusCode, err))
	}
}

func (m *ProviderProbeMonitor) pingExternalHeartbeat(ctx context.Context) {
	if m.beat == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, m.alertTimeout)
	defer cancel()
	resp, err := http.DefaultClient.Do(m.beat.Clone(ctx))
	if err != nil {
		// The heartbeat URL is a secret and *url.Error embeds it: log only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		slog.Warn("provider probe external heartbeat failed", "cause", err)
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 1024)
	resp.Body.Close()
}

// CheckHeartbeat alerts when no probe result was recorded within staleAfter.
// The key is the time of the last result, so each silence after a new result
// is its own incident.
func (m *ProviderProbeMonitor) CheckHeartbeat(ctx context.Context) {
	m.mu.Lock()
	last := m.lastResult
	m.mu.Unlock()
	if m.now().Sub(last) <= m.staleAfter {
		return
	}
	key := fmt.Sprintf("%s:heartbeat:%d", m.provider, last.Unix())
	m.alertOnce(ctx, key, fmt.Sprintf("provider %s probe silent since %s", m.provider, last.Format(time.RFC3339)))
}

func (m *ProviderProbeMonitor) alertOnce(ctx context.Context, key, message string) {
	m.mu.Lock()
	_, done := m.sent[key]
	m.mu.Unlock()
	if done {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, m.alertTimeout)
	defer cancel()
	if err := m.sink.Alert(ctx, key, message); err != nil {
		slog.Warn("provider probe alert failed", "key", key, "error", err)
		return // not marked sent: retried on the next tick
	}
	m.mu.Lock()
	m.sent[key] = struct{}{}
	m.mu.Unlock()
}

// WebhookAlertSink POSTs {"key","message"} as JSON to one URL. The URL may
// embed a secret: it is never logged or returned in errors.
type WebhookAlertSink struct {
	url    string
	client *http.Client
}

func NewWebhookAlertSink(rawURL string) (*WebhookAlertSink, error) {
	u, err := safeAlertURL(rawURL)
	if err != nil {
		return nil, err
	}
	return &WebhookAlertSink{url: u.String(), client: &http.Client{
		Timeout:       providerAlertTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (s *WebhookAlertSink) Alert(ctx context.Context, key, message string) error {
	body, err := json.Marshal(struct {
		Key     string `json:"key"`
		Message string `json:"message"`
	}{key, message})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("build alert webhook request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return errors.New("alert webhook request failed")
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 1024)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("alert webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// safeAlertURL: alert and heartbeat URLs may carry a secret, so they follow
// the bearer-token rule: https, or a loopback host.
func safeAlertURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || !tokenSafeURL(u) {
		return nil, errors.New("alert URL must be https, or http to a loopback host")
	}
	return u, nil
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

// NewProviderProbeMonitorFromEnv builds the production monitor. It returns
// (nil, nil) unless both the gateway (MULTICA_PROVIDER_RECOVERY_BASE_URL) and
// the alert webhook (MULTICA_PROVIDER_ALERT_WEBHOOK_URL) are set. Credential:
// MULTICA_PROVIDER_PROBE_CREDENTIAL or MULTICA_PROVIDER_PROBE_CREDENTIAL_FILE
// (read-only scope). Optional: MULTICA_PROVIDER_HEARTBEAT_URL (external
// dead-man's-switch), MULTICA_PROVIDER_PROBE_NAME, MULTICA_PROVIDER_PROBE_INTERVAL.
func NewProviderProbeMonitorFromEnv(getenv func(string) string) (*ProviderProbeMonitor, error) {
	baseURL, hook := strings.TrimSpace(getenv("MULTICA_PROVIDER_RECOVERY_BASE_URL")), strings.TrimSpace(getenv("MULTICA_PROVIDER_ALERT_WEBHOOK_URL"))
	if baseURL == "" || hook == "" {
		return nil, nil
	}
	probe, err := NewHTTPProviderFailureProbe(baseURL)
	if err != nil {
		return nil, err
	}
	token, err := LoadProviderProbeCredential(getenv("MULTICA_PROVIDER_PROBE_CREDENTIAL"), getenv("MULTICA_PROVIDER_PROBE_CREDENTIAL_FILE"))
	if err != nil {
		return nil, err
	}
	sink, err := NewWebhookAlertSink(hook)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(getenv("MULTICA_PROVIDER_PROBE_NAME"))
	if name == "" {
		name = "provider"
	}
	interval := 5 * time.Minute
	if raw := strings.TrimSpace(getenv("MULTICA_PROVIDER_PROBE_INTERVAL")); raw != "" {
		if interval, err = time.ParseDuration(raw); err != nil || interval <= 0 {
			return nil, errors.New("MULTICA_PROVIDER_PROBE_INTERVAL must be a positive duration")
		}
	}
	m := NewProviderProbeMonitor(name, probe.WithBearerToken(token), sink, time.Now, time.Hour, interval)
	if beat := strings.TrimSpace(getenv("MULTICA_PROVIDER_HEARTBEAT_URL")); beat != "" {
		if err := m.WithExternalHeartbeat(beat); err != nil {
			return nil, err
		}
	}
	return m, nil
}
