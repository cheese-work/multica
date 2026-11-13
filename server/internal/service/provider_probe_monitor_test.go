package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPProviderFailureProbeIssuesOnlyGET(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	probe, err := NewHTTPProviderFailureProbe(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	m, _, _ := newTestHeartbeat(probe.WithBearerToken("read-only-token"))
	m.RunOnce(context.Background())
	m.RunOnce(context.Background())
	if len(methods) != 2 {
		t.Fatalf("requests = %v, want 2", methods)
	}
	for _, method := range methods {
		if method != http.MethodGet {
			t.Fatalf("probe sent %s, want only GET", method)
		}
	}
}

func TestHTTPProviderFailureProbeSendsBearerToken(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer server.Close()
	probe, err := NewHTTPProviderFailureProbe(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.WithBearerToken("tok-123").Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestLoadProviderProbeCredential(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, env, path, want string
		wantErr               bool
	}{
		{"env wins", "from-env", file, "from-env", false},
		{"file outside repo", "", file, "from-file", false},
		{"none configured", "", "", "", false},
		{"relative path is a repo file", "", "go.mod", "", true},
		{"absolute path inside a git work tree", "", inRepoCredential(t), "", true},
		{"symlink into a git work tree", "", symlinkedRepoCredential(t), "", true},
		{"missing file", "", filepath.Join(t.TempDir(), "absent"), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := LoadProviderProbeCredential(c.env, c.path)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got %q err=%v, want %q wantErr=%t", got, err, c.want, c.wantErr)
			}
		})
	}
}

func inRepoCredential(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "config", "cred")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func symlinkedRepoCredential(t *testing.T) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(inRepoCredential(t), link); err != nil {
		t.Fatal(err)
	}
	return link
}

func TestHTTPProviderFailureProbeRefusesBearerTokenOverPlainHTTP(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer server.Close()
	// Non-loopback plain-HTTP host: the request must never leave the process.
	probe, err := NewHTTPProviderFailureProbe("http://gateway.example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.WithBearerToken("tok").Probe(context.Background()); !errors.Is(err, errBearerOverPlainHTTP) {
		t.Fatalf("err = %v, want errBearerOverPlainHTTP", err)
	}
	for _, base := range []string{"https://gateway.example.invalid", server.URL} { // https and loopback http are allowed
		p, err := NewHTTPProviderFailureProbe(base)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.WithBearerToken("tok").Probe(context.Background()); err != nil && errors.Is(err, errBearerOverPlainHTTP) {
			t.Fatalf("%s refused: %v", base, err)
		}
	}
}

func TestWebhookAlertSinkPostsKeyAndMessage(t *testing.T) {
	var method, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		method, body = r.Method, string(raw)
	}))
	defer server.Close()
	sink, err := NewWebhookAlertSink(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Alert(context.Background(), "k1", "down"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || body != `{"key":"k1","message":"down"}` {
		t.Fatalf("got %s %q", method, body)
	}
}

func TestWebhookAlertSinkErrorsAndPlainHTTPRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	sink, err := NewWebhookAlertSink(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Alert(context.Background(), "k", "m"); err == nil {
		t.Fatal("want error on 500")
	}
	if _, err := NewWebhookAlertSink("http://alerts.example.invalid/hook"); err == nil {
		t.Fatal("want refusal of plain http to a non-loopback host")
	}
}

// postRecorder is a webhook endpoint that records each POST body.
type postRecorder struct {
	mu    sync.Mutex
	posts []string
}

func (r *postRecorder) handler(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.posts = append(r.posts, string(raw))
	r.mu.Unlock()
}

func (r *postRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.posts...)
}

func TestWebhookProbeAlertSinkSendsOneWebhookPerIncident(t *testing.T) {
	rec := &postRecorder{}
	hook := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer hook.Close()
	webhook, err := NewWebhookAlertSink(hook.URL)
	if err != nil {
		t.Fatal(err)
	}
	probe := &scriptedProbe{err: errors.New("boom")}
	clock := &fakeProbeClock{now: probeT0}
	m := NewProbeHeartbeatMonitor(probe, NewWebhookProbeAlertSink("c00", webhook), clock.Now, probeInterval)

	m.RunOnce(context.Background())
	m.RunOnce(context.Background()) // same incident: no second webhook
	got := rec.snapshot()
	if len(got) != 1 || !strings.HasPrefix(got[0], `{"key":"c00:probe-gateway_unreachable:1","message":`) {
		t.Fatalf("webhook posts = %v, want one alert keyed c00:probe-gateway_unreachable:1", got)
	}

	probe.err, probe.evidence = nil, ProviderFailureProbeEvidence{StatusCode: http.StatusOK}
	m.RunOnce(context.Background()) // recovery: Clear sends nothing
	probe.err = errors.New("boom again")
	m.RunOnce(context.Background())
	got = rec.snapshot()
	if len(got) != 2 || !strings.HasPrefix(got[1], `{"key":"c00:probe-gateway_unreachable:2","message":`) {
		t.Fatalf("webhook posts = %v, want a second alert keyed c00:probe-gateway_unreachable:2", got)
	}
}

func TestWebhookProbeAlertSinkSendsSilenceAlertOnce(t *testing.T) {
	rec := &postRecorder{}
	hook := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer hook.Close()
	webhook, err := NewWebhookAlertSink(hook.URL)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeProbeClock{now: probeT0}
	m := NewProbeHeartbeatMonitor(&scriptedProbe{}, NewWebhookProbeAlertSink("c00", webhook), clock.Now, probeInterval)

	clock.now = probeT0.Add(probeInterval + time.Second)
	m.CheckSilence()
	m.CheckSilence()
	got := rec.snapshot()
	if len(got) != 1 || !strings.HasPrefix(got[0], `{"key":"c00:probe-silence:1","message":`) {
		t.Fatalf("webhook posts = %v, want one silence alert keyed c00:probe-silence:1", got)
	}
}

func TestWebhookProbeAlertSinkBoundsStalledWebhook(t *testing.T) {
	release := make(chan struct{})
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer hook.Close()
	defer close(release)
	webhook, err := NewWebhookAlertSink(hook.URL)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewWebhookProbeAlertSink("c00", webhook)
	sink.(*webhookProbeAlertSink).timeout = 50 * time.Millisecond
	done := make(chan struct{})
	go func() { sink.Raise("k", "gateway_unreachable", "down"); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Raise blocked on a stalled webhook")
	}
}

func TestProbeHeartbeatPingsExternalHeartbeatAfterEveryResult(t *testing.T) {
	var mu sync.Mutex
	var pings []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pings = append(pings, r.Method)
		mu.Unlock()
	}))
	defer server.Close()
	probe := &scriptedProbe{evidence: ProviderFailureProbeEvidence{StatusCode: http.StatusOK}}
	m, _, _ := newTestHeartbeat(probe)
	if err := m.WithExternalHeartbeat(server.URL); err != nil {
		t.Fatal(err)
	}
	m.RunOnce(context.Background())
	// A gateway outage is a probe result, not process death: the dead-man's
	// switch keeps getting pings so it alerts only when this process dies.
	probe.err = errors.New("gateway down")
	m.RunOnce(context.Background())
	probe.err, probe.evidence.StatusCode = nil, http.StatusBadGateway
	m.RunOnce(context.Background())
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	m.RunOnce(cancelled) // shutdown is not a probe result: no ping
	mu.Lock()
	defer mu.Unlock()
	if len(pings) != 3 || pings[0] != http.MethodGet {
		t.Fatalf("pings = %v, want 3 GETs", pings)
	}
}

func TestProbeHeartbeatRejectsUnsafeExternalHeartbeatURL(t *testing.T) {
	m, _, _ := newTestHeartbeat(&scriptedProbe{})
	if err := m.WithExternalHeartbeat("http://beat.example.invalid/ping"); err == nil {
		t.Fatal("want refusal of plain http to a non-loopback host")
	}
}

func TestProbeHeartbeatExternalHeartbeatFailureLogDoesNotLeakURLSecret(t *testing.T) {
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	secretURL := dead.URL + "/ping/s3cr3t-token?key=q-secret"
	dead.Close() // transport error that embeds the full URL
	m, _, _ := newTestHeartbeat(&scriptedProbe{evidence: ProviderFailureProbeEvidence{StatusCode: http.StatusOK}})
	if err := m.WithExternalHeartbeat(secretURL); err != nil {
		t.Fatal(err)
	}
	m.RunOnce(context.Background())

	if !strings.Contains(logs.String(), "heartbeat") {
		t.Fatalf("expected a heartbeat failure log, got %q", logs.String())
	}
	for _, secret := range []string{"s3cr3t-token", "q-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaks %q: %s", secret, logs.String())
		}
	}
}

func TestProbeHeartbeatMalformedRedirectLogDoesNotLeakURLSecret(t *testing.T) {
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	// Go embeds the full Location in the transport error for a malformed redirect.
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/ping/synthetic-path%zz?key=synthetic-query")
		w.WriteHeader(http.StatusFound)
	}))
	defer redirecting.Close()
	m, _, _ := newTestHeartbeat(&scriptedProbe{evidence: ProviderFailureProbeEvidence{StatusCode: http.StatusOK}})
	if err := m.WithExternalHeartbeat(redirecting.URL + "/ping/s3cr3t-token?key=q-secret"); err != nil {
		t.Fatal(err)
	}
	m.RunOnce(context.Background())

	if !strings.Contains(logs.String(), "heartbeat") {
		t.Fatalf("expected a heartbeat failure log, got %q", logs.String())
	}
	for _, secret := range []string{"s3cr3t-token", "q-secret", "synthetic-path", "synthetic-query"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaks %q: %s", secret, logs.String())
		}
	}
}

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestProbeHeartbeatFromEnvAlertsThroughWebhookWithCredential(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer env-token" {
			t.Errorf("unexpected probe request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer gateway.Close()
	rec := &postRecorder{}
	hook := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer hook.Close()
	probe, err := NewHTTPProviderFailureProbe(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}

	m, err := NewProbeHeartbeatMonitorFromEnv(envMap(map[string]string{
		"MULTICA_PROVIDER_PROBE_CREDENTIAL":  "env-token",
		"MULTICA_PROVIDER_ALERT_WEBHOOK_URL": hook.URL,
		"MULTICA_PROVIDER_PROBE_NAME":        "c00",
		"MULTICA_PROVIDER_HEARTBEAT_URL":     "http://127.0.0.1:1/ping/synthetic",
	}), probe, time.Now)
	if err != nil || m == nil {
		t.Fatalf("monitor=%v err=%v", m, err)
	}
	m.RunOnce(context.Background())
	m.RunOnce(context.Background())
	if got := rec.snapshot(); len(got) != 1 || !strings.Contains(got[0], `"key":"c00:probe-gateway_unreachable:1"`) {
		t.Fatalf("webhook posts = %v, want one gateway alert for c00", got)
	}
}

func TestProbeHeartbeatFromEnvWithoutOptionalEnvStaysSlogOnly(t *testing.T) {
	var auth atomic.Value
	auth.Store("unset")
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
	}))
	defer gateway.Close()
	probe, err := NewHTTPProviderFailureProbe(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewProbeHeartbeatMonitorFromEnv(envMap(nil), probe, time.Now)
	if err != nil || m == nil {
		t.Fatalf("monitor=%v err=%v, want the plain CHE-4576 monitor", m, err)
	}
	m.RunOnce(context.Background())
	if got := auth.Load(); got != "" {
		t.Fatalf("Authorization = %q, want none without a credential", got)
	}
	if _, ok := m.LastHeartbeat(); !ok {
		t.Fatal("probe did not run")
	}
}

func TestProbeHeartbeatFromEnvRejectsBadConfig(t *testing.T) {
	base := map[string]string{
		"MULTICA_PROVIDER_ALERT_WEBHOOK_URL": "http://127.0.0.1:1/hook",
		"MULTICA_PROVIDER_HEARTBEAT_URL":     "http://127.0.0.1:1/ping",
	}
	for name, extra := range map[string]map[string]string{
		"plain http webhook":       {"MULTICA_PROVIDER_ALERT_WEBHOOK_URL": "http://alerts.example.invalid/hook"},
		"relative credential":      {"MULTICA_PROVIDER_PROBE_CREDENTIAL_FILE": "go.mod"},
		"plain http heartbeat":     {"MULTICA_PROVIDER_HEARTBEAT_URL": "http://beat.example.invalid/ping"},
		"bad interval":             {"MULTICA_PROVIDER_PROBE_INTERVAL": "soon"},
		"zero interval":            {"MULTICA_PROVIDER_PROBE_INTERVAL": "0s"},
		"webhook without watchdog": {"MULTICA_PROVIDER_HEARTBEAT_URL": ""},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		if m, err := NewProbeHeartbeatMonitorFromEnv(envMap(env), &scriptedProbe{}, time.Now); err == nil || m != nil {
			t.Fatalf("%s: got monitor=%v err=%v, want error", name, m, err)
		}
	}
}

func TestProbeHeartbeatFromEnvRequiresExternalHeartbeatWithWebhook(t *testing.T) {
	// The in-process watchdog dies with the process, so a webhook-enabled monitor
	// needs an independently observed heartbeat.
	_, err := NewProbeHeartbeatMonitorFromEnv(envMap(map[string]string{
		"MULTICA_PROVIDER_ALERT_WEBHOOK_URL": "http://127.0.0.1:1/hook",
	}), &scriptedProbe{}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "MULTICA_PROVIDER_HEARTBEAT_URL") {
		t.Fatalf("err = %v, want one naming MULTICA_PROVIDER_HEARTBEAT_URL", err)
	}
}

func TestStartProviderProbeMonitorProbesAndPingsHeartbeat(t *testing.T) {
	var probes, beats atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { probes.Add(1) }))
	defer gateway.Close()
	beat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { beats.Add(1) }))
	defer beat.Close()
	probe, err := NewHTTPProviderFailureProbe(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !StartProviderProbeMonitor(ctx, envMap(map[string]string{
		"MULTICA_PROVIDER_PROBE_INTERVAL": "40ms",
		"MULTICA_PROVIDER_HEARTBEAT_URL":  beat.URL + "/ping/synthetic",
	}), probe) {
		t.Fatal("monitor did not start")
	}
	deadline := time.Now().Add(3 * time.Second)
	for (probes.Load() == 0 || beats.Load() == 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if probes.Load() == 0 || beats.Load() == 0 {
		t.Fatalf("probes=%d heartbeats=%d, want both > 0", probes.Load(), beats.Load())
	}
}

func TestStartProviderProbeMonitorFallsBackToSlogOnlyOnBadConfig(t *testing.T) {
	var probes atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { probes.Add(1) }))
	defer gateway.Close()
	probe, err := NewHTTPProviderFailureProbe(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A webhook without a heartbeat URL is a bad config; the CHE-4576 monitor still runs.
	if !StartProviderProbeMonitor(ctx, envMap(map[string]string{"MULTICA_PROVIDER_ALERT_WEBHOOK_URL": "http://127.0.0.1:1/hook"}), probe) {
		t.Fatal("monitor did not start")
	}
	deadline := time.Now().Add(3 * time.Second)
	for probes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if probes.Load() == 0 {
		t.Fatal("fallback monitor never probed")
	}
}

func TestStartProviderProbeMonitorStaysOffWithoutProbe(t *testing.T) {
	if StartProviderProbeMonitor(context.Background(), envMap(nil), nil) {
		t.Fatal("monitor started without a probe")
	}
}
