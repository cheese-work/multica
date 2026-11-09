package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeProbeClock struct{ now time.Time }

func (c *fakeProbeClock) Now() time.Time { return c.now }

type recordedAlert struct {
	Key, Class, Message string
}

type fakeAlertSink struct {
	raised  []recordedAlert
	cleared []string
}

func (s *fakeAlertSink) Raise(key, class, message string) {
	s.raised = append(s.raised, recordedAlert{key, class, message})
}
func (s *fakeAlertSink) Clear(key string) { s.cleared = append(s.cleared, key) }

type scriptedProbe struct {
	evidence ProviderFailureProbeEvidence
	err      error
}

func (p *scriptedProbe) Probe(context.Context) (ProviderFailureProbeEvidence, error) {
	return p.evidence, p.err
}

const probeInterval = 10 * time.Minute

var probeT0 = time.Date(2026, 11, 8, 3, 0, 0, 0, time.UTC)

func newTestHeartbeat(probe ProviderFailureProbe) (*ProbeHeartbeatMonitor, *fakeProbeClock, *fakeAlertSink) {
	clock := &fakeProbeClock{now: probeT0}
	sink := &fakeAlertSink{}
	return NewProbeHeartbeatMonitor(probe, sink, clock.Now, probeInterval), clock, sink
}

func TestProbeHeartbeat_RunRecordsHeartbeatTime(t *testing.T) {
	m, clock, _ := newTestHeartbeat(&scriptedProbe{evidence: ProviderFailureProbeEvidence{StatusCode: 401}})
	if _, ok := m.LastHeartbeat(); ok {
		t.Fatal("heartbeat recorded before any probe run")
	}
	clock.now = probeT0.Add(3 * time.Minute)
	m.RunOnce(context.Background())
	got, ok := m.LastHeartbeat()
	if !ok || !got.Equal(time.Date(2026, 11, 8, 3, 3, 0, 0, time.UTC)) {
		t.Fatalf("LastHeartbeat = %v, %v; want 2026-11-08T03:03:00Z, true", got, ok)
	}
}

func TestProbeHeartbeat_SilenceRaisesExactlyOneAlert(t *testing.T) {
	m, clock, sink := newTestHeartbeat(&scriptedProbe{evidence: ProviderFailureProbeEvidence{StatusCode: 401}})
	m.RunOnce(context.Background())
	clock.now = probeT0.Add(9 * time.Minute)
	m.CheckSilence()
	if len(sink.raised) != 0 {
		t.Fatalf("alert before interval elapsed: %+v", sink.raised)
	}
	for i := 0; i < 4; i++ {
		clock.now = probeT0.Add(11*time.Minute + time.Duration(i)*time.Minute)
		m.CheckSilence()
	}
	if len(sink.raised) != 1 {
		t.Fatalf("raised %d alerts, want exactly 1: %+v", len(sink.raised), sink.raised)
	}
	if got := sink.raised[0]; got.Key != "probe-silence:1" || got.Class != "heartbeat_silence" {
		t.Fatalf("alert = %+v, want key probe-silence:1 class heartbeat_silence", got)
	}
}

func TestProbeHeartbeat_NeverRunAlertsAfterInterval(t *testing.T) {
	m, clock, sink := newTestHeartbeat(&scriptedProbe{})
	clock.now = probeT0.Add(11 * time.Minute)
	m.CheckSilence()
	if len(sink.raised) != 1 {
		t.Fatalf("raised %d alerts, want 1 when the probe never ran past the interval", len(sink.raised))
	}
}

func TestProbeHeartbeat_HeartbeatClearsAndSecondSilenceRaisesNewKey(t *testing.T) {
	m, clock, sink := newTestHeartbeat(&scriptedProbe{evidence: ProviderFailureProbeEvidence{StatusCode: 401}})
	m.RunOnce(context.Background())
	clock.now = probeT0.Add(11 * time.Minute)
	m.CheckSilence()

	clock.now = probeT0.Add(12 * time.Minute)
	m.RunOnce(context.Background())
	if len(sink.cleared) != 1 || sink.cleared[0] != "probe-silence:1" {
		t.Fatalf("cleared = %v, want [probe-silence:1]", sink.cleared)
	}

	clock.now = probeT0.Add(23 * time.Minute)
	m.CheckSilence()
	m.CheckSilence()
	if len(sink.raised) != 2 || sink.raised[1].Key != "probe-silence:2" {
		t.Fatalf("raised = %+v, want a second alert with key probe-silence:2", sink.raised)
	}
}

func TestProbeHeartbeat_FailureClassesStayDistinct(t *testing.T) {
	cases := []struct {
		name      string
		probe     *scriptedProbe
		wantClass string
		wantKey   string
		wantMsg   string
	}{
		{
			name:      "unreachable gateway",
			probe:     &scriptedProbe{err: errors.New("request provider recovery probe: dial tcp 100.64.0.9:31344: connect: connection refused")},
			wantClass: "gateway_unreachable",
			wantKey:   "probe-gateway_unreachable:1",
			wantMsg:   "request provider recovery probe: dial tcp 100.64.0.9:31344: connect: connection refused",
		},
		{
			name:      "account fault",
			probe:     &scriptedProbe{evidence: ProviderFailureProbeEvidence{StatusCode: 403}},
			wantClass: "account_fault",
			wantKey:   "probe-account_fault:1",
			wantMsg:   "gateway answered HTTP 403",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, sink := newTestHeartbeat(tc.probe)
			m.RunOnce(context.Background())
			m.RunOnce(context.Background())
			if len(sink.raised) != 1 {
				t.Fatalf("raised %d alerts, want 1 across repeated failing runs: %+v", len(sink.raised), sink.raised)
			}
			want := recordedAlert{tc.wantKey, tc.wantClass, tc.wantMsg}
			if sink.raised[0] != want {
				t.Fatalf("alert = %+v, want %+v", sink.raised[0], want)
			}
			if _, ok := m.LastHeartbeat(); !ok {
				t.Fatal("a failing probe run still proves the probe loop is alive; heartbeat missing")
			}
		})
	}
}

func TestProbeHeartbeat_HealthyRunClearsFailureAlert(t *testing.T) {
	probe := &scriptedProbe{err: errors.New("request provider recovery probe: connection refused")}
	m, _, sink := newTestHeartbeat(probe)
	m.RunOnce(context.Background())
	probe.err, probe.evidence = nil, ProviderFailureProbeEvidence{StatusCode: 401}
	m.RunOnce(context.Background())
	if len(sink.cleared) != 1 || sink.cleared[0] != "probe-gateway_unreachable:1" {
		t.Fatalf("cleared = %v, want [probe-gateway_unreachable:1]", sink.cleared)
	}
}

type blockingProbe struct{ release chan struct{} }

func (p *blockingProbe) Probe(ctx context.Context) (ProviderFailureProbeEvidence, error) {
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return ProviderFailureProbeEvidence{}, ctx.Err()
}

// lockedAlertSink is a goroutine-safe sink for tests that drive Run.
type lockedAlertSink struct {
	mu sync.Mutex
	fakeAlertSink
}

func (s *lockedAlertSink) Raise(key, class, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fakeAlertSink.Raise(key, class, message)
}
func (s *lockedAlertSink) raisedCopy() []recordedAlert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedAlert(nil), s.raised...)
}

func TestProbeHeartbeat_RunAlertsWhileProbeIsBlocked(t *testing.T) {
	sink := &lockedAlertSink{}
	m := NewProbeHeartbeatMonitor(&blockingProbe{release: make(chan struct{})}, sink, time.Now, 40*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for len(sink.raisedCopy()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // more watchdog ticks must not re-raise
	cancel()
	<-done

	got := sink.raisedCopy()
	if len(got) != 1 || got[0].Key != "probe-silence:1" || got[0].Class != "heartbeat_silence" {
		t.Fatalf("raised = %+v, want exactly one probe-silence:1 alert while the probe is blocked", got)
	}
}
