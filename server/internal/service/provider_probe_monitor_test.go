package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingAlertSink struct {
	mu   sync.Mutex
	keys []string
	err  error
}

func (s *recordingAlertSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...)
}

func (s *recordingAlertSink) Alert(_ context.Context, key, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.keys = append(s.keys, key)
	return nil
}

type scriptedProbe struct {
	status int
	err    error
}

func (p *scriptedProbe) Probe(context.Context) (ProviderFailureProbeEvidence, error) {
	return ProviderFailureProbeEvidence{CheckedAt: time.Unix(0, 0), StatusCode: p.status}, p.err
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

var monitorT0 = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

func newTestMonitor(probe ProviderFailureProbe) (*ProviderProbeMonitor, *recordingAlertSink, *fakeClock) {
	clock := &fakeClock{now: monitorT0}
	sink := &recordingAlertSink{}
	m := NewProviderProbeMonitor("openai", probe, sink, clock.Now, time.Hour, 5*time.Minute)
	return m, sink, clock
}

func TestProviderProbeMonitorAlertsOncePerIncident(t *testing.T) {
	probe := &scriptedProbe{status: http.StatusBadGateway}
	m, sink, clock := newTestMonitor(probe)

	m.Tick(context.Background())
	clock.now = monitorT0.Add(10 * time.Minute)
	m.Tick(context.Background())
	clock.now = monitorT0.Add(50 * time.Minute)
	m.Tick(context.Background())

	if len(sink.keys) != 1 || sink.keys[0] != "openai:failure:1794132000:1" {
		t.Fatalf("alert keys = %v, want exactly [openai:failure:1794132000:1]", sink.keys)
	}
}

func TestProviderProbeMonitorRetriesAlertWhenSinkFails(t *testing.T) {
	m, sink, _ := newTestMonitor(&scriptedProbe{err: errors.New("boom")})
	sink.err = errors.New("sink down")
	m.Tick(context.Background())
	sink.err = nil
	m.Tick(context.Background())
	if len(sink.keys) != 1 {
		t.Fatalf("alert keys = %v, want one after sink recovers", sink.keys)
	}
}

func TestProviderProbeMonitorHealthyProbeDoesNotAlert(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		m, sink, clock := newTestMonitor(&scriptedProbe{status: status})
		clock.now = monitorT0.Add(4 * time.Minute)
		m.Tick(context.Background())
		if len(sink.keys) != 0 {
			t.Fatalf("status %d alert keys = %v, want none", status, sink.keys)
		}
	}
}

func TestProviderProbeMonitorHeartbeatAlertsOnSilenceOnce(t *testing.T) {
	m, sink, clock := newTestMonitor(&scriptedProbe{status: http.StatusOK})

	clock.now = monitorT0.Add(4 * time.Minute)
	m.CheckHeartbeat(context.Background())
	if len(sink.keys) != 0 {
		t.Fatalf("alert before interval elapsed: %v", sink.keys)
	}
	clock.now = monitorT0.Add(6 * time.Minute)
	m.CheckHeartbeat(context.Background())
	clock.now = monitorT0.Add(9 * time.Minute)
	m.CheckHeartbeat(context.Background())

	if len(sink.keys) != 1 || sink.keys[0] != "openai:heartbeat:1794132000" {
		t.Fatalf("alert keys = %v, want exactly [openai:heartbeat:1794132000]", sink.keys)
	}
}

func TestProviderProbeMonitorResultResetsHeartbeat(t *testing.T) {
	m, sink, clock := newTestMonitor(&scriptedProbe{status: http.StatusOK})
	clock.now = monitorT0.Add(4 * time.Minute)
	m.Tick(context.Background())
	clock.now = monitorT0.Add(8 * time.Minute)
	m.CheckHeartbeat(context.Background())
	if len(sink.keys) != 0 {
		t.Fatalf("alert keys = %v, want none: last result was 4 minutes ago", sink.keys)
	}
}

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
	m, _, _ := newTestMonitor(probe.WithBearerToken("read-only-token"))
	m.Tick(context.Background())
	m.Tick(context.Background())
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

func TestProviderProbeMonitorAlertsAgainForNewEpisodeInSameWindow(t *testing.T) {
	probe := &scriptedProbe{status: http.StatusBadGateway}
	m, sink, clock := newTestMonitor(probe)

	m.Tick(context.Background()) // episode 1 at 10:00
	probe.status = http.StatusOK
	clock.now = monitorT0.Add(10 * time.Minute)
	m.Tick(context.Background()) // recovery
	probe.status = http.StatusBadGateway
	clock.now = monitorT0.Add(20 * time.Minute)
	m.Tick(context.Background()) // episode 2, same one-hour window

	want := []string{"openai:failure:1794132000:1", "openai:failure:1794132000:2"}
	if got := sink.snapshot(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("alert keys = %v, want %v", got, want)
	}
}

func TestProviderProbeMonitorHeartbeatAlertsAgainAfterInterveningResult(t *testing.T) {
	m, sink, clock := newTestMonitor(&scriptedProbe{status: http.StatusOK})

	clock.now = monitorT0.Add(6 * time.Minute)
	m.CheckHeartbeat(context.Background()) // silence 1
	m.Tick(context.Background())           // result at 10:06 ends it
	clock.now = monitorT0.Add(13 * time.Minute)
	m.CheckHeartbeat(context.Background()) // silence 2, same one-hour window

	want := []string{"openai:heartbeat:1794132000", "openai:heartbeat:1794132360"}
	if got := sink.snapshot(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("alert keys = %v, want %v", got, want)
	}
}

type blockingProbe struct{ release chan struct{} }

func (p *blockingProbe) Probe(ctx context.Context) (ProviderFailureProbeEvidence, error) {
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return ProviderFailureProbeEvidence{}, errors.New("stalled")
}

func TestProviderProbeMonitorHeartbeatFiresWhileProbeIsStalled(t *testing.T) {
	probe := &blockingProbe{release: make(chan struct{})}
	defer close(probe.release)
	sink := &recordingAlertSink{}
	m := NewProviderProbeMonitor("openai", probe, sink, time.Now, time.Hour, 80*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, k := range sink.snapshot() {
			if strings.Contains(k, ":heartbeat:") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no heartbeat alert while probe stalled; keys = %v", sink.snapshot())
}

type blockingSink struct{}

func (blockingSink) Alert(ctx context.Context, _, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestProviderProbeMonitorBoundsStalledAlertSink(t *testing.T) {
	m := NewProviderProbeMonitor("openai", &scriptedProbe{status: http.StatusBadGateway}, blockingSink{}, time.Now, time.Hour, time.Minute)
	m.alertTimeout = 50 * time.Millisecond
	done := make(chan struct{})
	go func() { m.Tick(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Tick blocked on a stalled alert sink")
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

func TestProviderProbeMonitorPingsExternalHeartbeatOnEachResult(t *testing.T) {
	var mu sync.Mutex
	var pings []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pings = append(pings, r.Method)
		mu.Unlock()
	}))
	defer server.Close()
	m, _, _ := newTestMonitor(&scriptedProbe{status: http.StatusOK})
	if err := m.WithExternalHeartbeat(server.URL); err != nil {
		t.Fatal(err)
	}
	m.Tick(context.Background())
	m.Tick(context.Background())
	probeFails, _, _ := newTestMonitor(&scriptedProbe{err: errors.New("boom")})
	if err := probeFails.WithExternalHeartbeat(server.URL); err != nil {
		t.Fatal(err)
	}
	probeFails.Tick(context.Background()) // no result recorded: no ping
	if len(pings) != 2 || pings[0] != http.MethodGet {
		t.Fatalf("pings = %v, want 2 GETs", pings)
	}
}

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestProviderProbeMonitorFromEnvDisabledUnlessGatewayAndWebhookSet(t *testing.T) {
	for _, env := range []map[string]string{
		{},
		{"MULTICA_PROVIDER_RECOVERY_BASE_URL": "http://127.0.0.1:1"},
		{"MULTICA_PROVIDER_ALERT_WEBHOOK_URL": "http://127.0.0.1:1/hook"},
	} {
		m, err := NewProviderProbeMonitorFromEnv(envMap(env))
		if m != nil || err != nil {
			t.Fatalf("env %v: got monitor=%v err=%v, want disabled", env, m, err)
		}
	}
}

func TestProviderProbeMonitorFromEnvAlertsOnGatewayFailureThroughWebhook(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer env-token" {
			t.Errorf("unexpected probe request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer gateway.Close()
	var mu sync.Mutex
	var posts []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, string(raw))
		mu.Unlock()
	}))
	defer hook.Close()

	m, err := NewProviderProbeMonitorFromEnv(envMap(map[string]string{
		"MULTICA_PROVIDER_RECOVERY_BASE_URL": gateway.URL,
		"MULTICA_PROVIDER_PROBE_CREDENTIAL":  "env-token",
		"MULTICA_PROVIDER_ALERT_WEBHOOK_URL": hook.URL,
		"MULTICA_PROVIDER_PROBE_NAME":        "c00",
	}))
	if err != nil || m == nil {
		t.Fatalf("monitor=%v err=%v", m, err)
	}
	m.Tick(context.Background())
	m.Tick(context.Background())
	if len(posts) != 1 || !strings.Contains(posts[0], `"key":"c00:failure:`) {
		t.Fatalf("webhook posts = %v, want one failure alert for c00", posts)
	}
}

func TestProviderProbeMonitorFromEnvRejectsBadConfig(t *testing.T) {
	base := map[string]string{
		"MULTICA_PROVIDER_RECOVERY_BASE_URL": "http://127.0.0.1:1",
		"MULTICA_PROVIDER_ALERT_WEBHOOK_URL": "http://127.0.0.1:1/hook",
	}
	for name, extra := range map[string]map[string]string{
		"plain http webhook":   {"MULTICA_PROVIDER_ALERT_WEBHOOK_URL": "http://alerts.example.invalid/hook"},
		"relative credential":  {"MULTICA_PROVIDER_PROBE_CREDENTIAL_FILE": "go.mod"},
		"plain http heartbeat": {"MULTICA_PROVIDER_HEARTBEAT_URL": "http://beat.example.invalid/ping"},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		if m, err := NewProviderProbeMonitorFromEnv(envMap(env)); err == nil || m != nil {
			t.Fatalf("%s: got monitor=%v err=%v, want error", name, m, err)
		}
	}
}
