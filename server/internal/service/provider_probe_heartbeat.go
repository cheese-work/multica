package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	probeAlertSilence     = "heartbeat_silence"
	probeAlertUnreachable = "gateway_unreachable"
	probeAlertAccount     = "account_fault"
)

// ProbeAlertSink receives per-incident alerts. Raise is called once per
// incident key; Clear is called once when that incident recovers.
type ProbeAlertSink interface {
	Raise(key, class, message string)
	Clear(key string)
}

// SlogProbeAlertSink logs alerts at error level; log shippers key on "alert_key".
type SlogProbeAlertSink struct{}

func (SlogProbeAlertSink) Raise(key, class, message string) {
	slog.Error("provider probe alert", "alert_key", key, "alert_class", class, "message", message)
}

func (SlogProbeAlertSink) Clear(key string) {
	slog.Info("provider probe alert cleared", "alert_key", key)
}

// ProbeHeartbeatMonitor runs the read-only provider probe, records a heartbeat
// per run, and alerts when heartbeats stop or the probe finds a fault.
type ProbeHeartbeatMonitor struct {
	probe    ProviderFailureProbe
	sink     ProbeAlertSink
	now      func() time.Time
	interval time.Duration
	beat     *http.Request // external dead-man's-switch GET, nil when unset

	mu        sync.Mutex
	started   time.Time
	last      time.Time
	hasLast   bool
	incidents map[string]int // per class: count of incidents raised so far
	openKeys  map[string]string
}

func NewProbeHeartbeatMonitor(probe ProviderFailureProbe, sink ProbeAlertSink, now func() time.Time, interval time.Duration) *ProbeHeartbeatMonitor {
	return &ProbeHeartbeatMonitor{
		probe: probe, sink: sink, now: now, interval: interval, started: now(),
		incidents: map[string]int{}, openKeys: map[string]string{},
	}
}

func (m *ProbeHeartbeatMonitor) LastHeartbeat() (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last, m.hasLast
}

// WithExternalHeartbeat pings rawURL (GET) after every recorded probe result.
// Point it at a dead-man's-switch service: it alerts when pings stop, which
// the in-process silence check cannot do if the process dies. The URL may carry
// a secret, so it must be https or loopback and is never logged.
func (m *ProbeHeartbeatMonitor) WithExternalHeartbeat(rawURL string) error {
	u, err := safeAlertURL(rawURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("build heartbeat request") // never wrap err: it can embed the secret URL
	}
	m.beat = req
	return nil
}

func (m *ProbeHeartbeatMonitor) pingExternalHeartbeat(ctx context.Context) {
	if m.beat == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, providerAlertTimeout)
	defer cancel()
	resp, err := http.DefaultClient.Do(m.beat.Clone(ctx))
	if err != nil {
		// *url.Error embeds the secret URL and the cause can embed a redirect
		// Location, so log a fixed category, never the error text.
		slog.Warn("provider probe external heartbeat failed", "category", heartbeatFailureCategory(err))
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 1024)
	resp.Body.Close()
}

// heartbeatFailureCategory maps a heartbeat transport error to a fixed label.
func heartbeatFailureCategory(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "transport"
	}
}

// RunOnce probes the gateway. The run itself is the heartbeat, healthy or not;
// a failing probe raises its own class of alert.
func (m *ProbeHeartbeatMonitor) RunOnce(ctx context.Context) {
	evidence, err := m.probe.Probe(ctx)
	if ctx.Err() != nil {
		return // shutdown, not a probe result
	}
	class, message := classifyProbeResult(evidence, err)
	m.record(class, message)
	// The ping proves this process is alive, so a gateway outage still pings.
	m.pingExternalHeartbeat(ctx)
}

func (m *ProbeHeartbeatMonitor) record(class, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last, m.hasLast = m.now(), true
	m.clearLocked(probeAlertSilence)
	for _, c := range []string{probeAlertUnreachable, probeAlertAccount} {
		if c != class {
			m.clearLocked(c)
		}
	}
	if class != "" {
		m.raiseLocked(class, message)
	}
}

// CheckSilence raises one alert per silence incident once no heartbeat has
// landed for the interval. A monitor that never ran counts from its creation.
func (m *ProbeHeartbeatMonitor) CheckSilence() {
	m.mu.Lock()
	defer m.mu.Unlock()
	since := m.started
	if m.hasLast {
		since = m.last
	}
	if m.now().Sub(since) > m.interval {
		m.raiseLocked(probeAlertSilence, fmt.Sprintf("no probe heartbeat since %s (interval %s)", since.UTC().Format(time.RFC3339), m.interval))
	}
}

// Run probes every interval/2 and checks silence on its own goroutine, so a
// blocked probe cannot suppress the silence alert.
func (m *ProbeHeartbeatMonitor) Run(ctx context.Context) {
	tick := m.interval / 2
	go func() {
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.CheckSilence()
			}
		}
	}()
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		m.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *ProbeHeartbeatMonitor) raiseLocked(class, message string) {
	if m.openKeys[class] != "" {
		return
	}
	m.incidents[class]++
	name := class
	if class == probeAlertSilence {
		name = "silence"
	}
	key := fmt.Sprintf("probe-%s:%d", name, m.incidents[class])
	m.openKeys[class] = key
	m.sink.Raise(key, class, message)
}

func (m *ProbeHeartbeatMonitor) clearLocked(class string) {
	if key := m.openKeys[class]; key != "" {
		delete(m.openKeys, class)
		m.sink.Clear(key)
	}
}

// classifyProbeResult: 2xx and 401 are healthy (401 = auth required, gateway up).
func classifyProbeResult(e ProviderFailureProbeEvidence, err error) (class, message string) {
	switch {
	case err != nil:
		return probeAlertUnreachable, err.Error()
	case e.StatusCode >= 200 && e.StatusCode < 300, e.StatusCode == http.StatusUnauthorized:
		return "", ""
	case e.StatusCode == http.StatusPaymentRequired, e.StatusCode == http.StatusForbidden, e.StatusCode == http.StatusTooManyRequests:
		return probeAlertAccount, fmt.Sprintf("gateway answered HTTP %d", e.StatusCode)
	default:
		return probeAlertUnreachable, fmt.Sprintf("gateway answered HTTP %d", e.StatusCode)
	}
}
