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
	"time"
)

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

const (
	providerAlertTimeout = 5 * time.Second
	defaultProbeInterval = 10 * time.Minute
)

// webhookProbeAlertSink logs each alert like SlogProbeAlertSink and also POSTs
// it to the webhook, with the provider name as key prefix so one receiver can
// tell gateways apart. A failed delivery is logged and not retried: Raise runs
// once per incident. Clear sends nothing.
type webhookProbeAlertSink struct {
	SlogProbeAlertSink
	name    string
	webhook *WebhookAlertSink
	timeout time.Duration
}

func NewWebhookProbeAlertSink(name string, w *WebhookAlertSink) ProbeAlertSink {
	return &webhookProbeAlertSink{name: name, webhook: w, timeout: providerAlertTimeout}
}

func (s *webhookProbeAlertSink) Raise(key, class, message string) {
	s.SlogProbeAlertSink.Raise(key, class, message)
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	if err := s.webhook.Alert(ctx, s.name+":"+key, message); err != nil {
		slog.Warn("provider probe alert webhook failed", "key", key, "error", err)
	}
}

// NewProbeHeartbeatMonitorFromEnv builds the CHE-4576 monitor with the CHE-4580
// opt-ins, all read from env and all off when unset:
//   - MULTICA_PROVIDER_ALERT_WEBHOOK_URL: also POST each alert (key prefix:
//     MULTICA_PROVIDER_PROBE_NAME, default "provider"). Needs the heartbeat URL.
//   - MULTICA_PROVIDER_HEARTBEAT_URL: external dead-man's-switch, pinged after
//     every probe result.
//   - MULTICA_PROVIDER_PROBE_CREDENTIAL or _FILE: read-only bearer token for the
//     probe (GET /v1/models only).
//   - MULTICA_PROVIDER_PROBE_INTERVAL: silence interval, default 10m.
func NewProbeHeartbeatMonitorFromEnv(getenv func(string) string, probe ProviderFailureProbe, now func() time.Time) (*ProbeHeartbeatMonitor, error) {
	token, err := LoadProviderProbeCredential(getenv("MULTICA_PROVIDER_PROBE_CREDENTIAL"), getenv("MULTICA_PROVIDER_PROBE_CREDENTIAL_FILE"))
	if err != nil {
		return nil, err
	}
	if h, ok := probe.(*HTTPProviderFailureProbe); ok && token != "" {
		probe = h.WithBearerToken(token)
	}
	interval := defaultProbeInterval
	if raw := strings.TrimSpace(getenv("MULTICA_PROVIDER_PROBE_INTERVAL")); raw != "" {
		if interval, err = time.ParseDuration(raw); err != nil || interval <= 0 {
			return nil, errors.New("MULTICA_PROVIDER_PROBE_INTERVAL must be a positive duration")
		}
	}
	var sink ProbeAlertSink = SlogProbeAlertSink{}
	hook, beat := strings.TrimSpace(getenv("MULTICA_PROVIDER_ALERT_WEBHOOK_URL")), strings.TrimSpace(getenv("MULTICA_PROVIDER_HEARTBEAT_URL"))
	if hook != "" {
		// The in-process silence check dies with the process, so webhook alerting
		// needs an independently observed heartbeat.
		if beat == "" {
			return nil, errors.New("MULTICA_PROVIDER_HEARTBEAT_URL is required when MULTICA_PROVIDER_ALERT_WEBHOOK_URL is set")
		}
		webhook, err := NewWebhookAlertSink(hook)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(getenv("MULTICA_PROVIDER_PROBE_NAME"))
		if name == "" {
			name = "provider"
		}
		sink = NewWebhookProbeAlertSink(name, webhook)
	}
	m := NewProbeHeartbeatMonitor(probe, sink, now, interval)
	if beat != "" {
		if err := m.WithExternalHeartbeat(beat); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// StartProviderProbeMonitor runs the monitor on its own goroutine until ctx
// ends and reports whether one started. A bad configuration is logged and the
// plain CHE-4576 monitor (slog alerts, no credential, no heartbeat) runs instead,
// so a typo cannot switch silence alerting off.
func StartProviderProbeMonitor(ctx context.Context, getenv func(string) string, probe ProviderFailureProbe) bool {
	if probe == nil {
		return false
	}
	m, err := NewProbeHeartbeatMonitorFromEnv(getenv, probe, time.Now)
	if err != nil {
		slog.Error("provider probe monitor configuration is invalid; running with defaults", "error", err)
		m = NewProbeHeartbeatMonitor(probe, SlogProbeAlertSink{}, time.Now, defaultProbeInterval)
	}
	go m.Run(ctx)
	return true
}
