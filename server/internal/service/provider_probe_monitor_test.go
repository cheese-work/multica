package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type recordingAlertSink struct {
	mu   sync.Mutex
	keys []string
	err  error
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

	if len(sink.keys) != 1 || sink.keys[0] != "openai:failure:1794132000" {
		t.Fatalf("alert keys = %v, want exactly [openai:failure:1794132000]", sink.keys)
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
