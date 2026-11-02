package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type silenceRig struct {
	w      *silenceWatchdog
	now    time.Duration
	last   atomic.Int64
	tools  int32
	calls  []toolCall
	marks  []string
	alerts []string
	err    error
}

func newSilenceRig(warn, alert time.Duration) *silenceRig {
	r := &silenceRig{}
	r.w = &silenceWatchdog{
		warn: warn, alert: alert, last: &r.last,
		inFlight: func() int32 { return r.tools },
		calls:    func() []toolCall { return r.calls },
		now:      func() time.Duration { return r.now },
		mark: func(typ, content, callID, tool string) {
			r.marks = append(r.marks, typ+":"+callID)
		},
		raise: func(seg string, d time.Duration) error {
			r.alerts = append(r.alerts, seg)
			return r.err
		},
		log: slog.Default(),
	}
	return r
}

// advance moves the awake clock and runs one watchdog tick.
func (r *silenceRig) advance(d time.Duration) {
	r.now += d
	r.w.step()
}

// message simulates a backend message: activity stamp moves to now.
func (r *silenceRig) message() { r.last.Store(int64(r.now)) }

func (r *silenceRig) want(t *testing.T, marks, alerts int) {
	t.Helper()
	if len(r.marks) != marks || len(r.alerts) != alerts {
		t.Fatalf("marks=%v alerts=%v, want %d marks %d alerts", r.marks, r.alerts, marks, alerts)
	}
}

func TestSilenceFiresOncePerTierPerSegment(t *testing.T) {
	r := newSilenceRig(15*time.Minute, time.Hour)
	r.advance(14 * time.Minute)
	r.want(t, 0, 0)
	r.advance(2 * time.Minute)
	r.want(t, 1, 0)
	r.advance(10 * time.Minute)
	r.want(t, 1, 0)
	r.advance(34 * time.Minute) // 60m silent
	r.want(t, 1, 1)
	r.advance(time.Hour)
	r.want(t, 1, 1)
}

func TestSilenceSuppressedWhileToolRunsAndMarksStalledToolOnce(t *testing.T) {
	r := newSilenceRig(15*time.Minute, time.Hour)
	r.tools = 1
	r.calls = []toolCall{{id: "c1", tool: "Bash", since: 0}}
	r.advance(10 * time.Minute)
	r.want(t, 0, 0)
	r.advance(10 * time.Minute)
	r.want(t, 1, 0)
	if r.marks[0] != "tool_stalled:c1" {
		t.Fatalf("mark = %q", r.marks[0])
	}
	r.advance(2 * time.Hour)
	r.want(t, 1, 0)
}

func TestSilenceMessageRearmsBothTiers(t *testing.T) {
	r := newSilenceRig(15*time.Minute, time.Hour)
	r.advance(time.Hour)
	r.want(t, 1, 1)
	r.message()
	r.advance(time.Minute)
	r.want(t, 1, 1)
	r.advance(14 * time.Minute)
	r.want(t, 2, 1)
	r.advance(45 * time.Minute)
	r.want(t, 2, 2)
	if r.alerts[0] == r.alerts[1] {
		t.Fatalf("segments must differ: %v", r.alerts)
	}
}

func TestSilenceTiersDisableIndependently(t *testing.T) {
	r := newSilenceRig(0, time.Hour)
	r.advance(30 * time.Minute)
	r.want(t, 0, 0)
	r.advance(30 * time.Minute)
	r.want(t, 0, 1)

	r = newSilenceRig(15*time.Minute, 0)
	r.advance(2 * time.Hour)
	r.want(t, 1, 0)

	r = newSilenceRig(0, 0)
	r.tools = 1
	r.calls = []toolCall{{id: "c1", since: 0}}
	r.advance(2 * time.Hour)
	r.want(t, 0, 0)
}

func TestSilenceAlertErrorIsNotRetriedAndDoesNotPanic(t *testing.T) {
	r := newSilenceRig(15*time.Minute, time.Hour)
	r.err = fmt.Errorf("server down")
	r.advance(time.Hour)
	r.advance(time.Hour)
	r.want(t, 1, 1)
}

func TestToolCallRegistryTracksRunningCalls(t *testing.T) {
	var reg toolCallRegistry
	reg.start("a", "Bash", time.Minute)
	reg.start("b", "Read", 2*time.Minute)
	reg.finish("a")
	got := reg.running()
	if len(got) != 1 || got[0].id != "b" || got[0].tool != "Read" || got[0].since != 2*time.Minute {
		t.Fatalf("running = %+v", got)
	}
}

func TestResolveSilenceTiers(t *testing.T) {
	cases := []struct {
		name                      string
		warn, alert, wantW, wantA time.Duration
		clamped                   bool
	}{
		{"defaults", 15 * time.Minute, time.Hour, 15 * time.Minute, time.Hour, false},
		{"alert below warn clamps", time.Hour, 15 * time.Minute, time.Hour, time.Hour, true},
		{"warn off keeps alert", 0, time.Minute, 0, time.Minute, false},
		{"alert off keeps warn", time.Hour, 0, time.Hour, 0, false},
	}
	for _, c := range cases {
		w, a, clamped := resolveSilenceTiers(c.warn, c.alert)
		if w != c.wantW || a != c.wantA || clamped != c.clamped {
			t.Errorf("%s: got %s %s %v", c.name, w, a, clamped)
		}
	}
}

func TestLoadConfig_SilenceWatchdog(t *testing.T) {
	stageFakeAgent(t)
	load := func() Config {
		t.Helper()
		cfg, err := LoadConfig(Overrides{ServerURL: "http://localhost:8080", WorkspacesRoot: t.TempDir()})
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		return cfg
	}
	check := func(name string, warn, alert time.Duration) {
		t.Helper()
		cfg := load()
		if cfg.AgentSilenceWarnAfter != warn || cfg.AgentSilenceAlertAfter != alert {
			t.Fatalf("%s: warn=%s alert=%s, want %s %s", name, cfg.AgentSilenceWarnAfter, cfg.AgentSilenceAlertAfter, warn, alert)
		}
	}
	t.Setenv("MULTICA_AGENT_SILENCE_WARN_AFTER", "")
	t.Setenv("MULTICA_AGENT_SILENCE_ALERT_AFTER", "")
	check("defaults", 15*time.Minute, time.Hour)

	t.Setenv("MULTICA_AGENT_SILENCE_WARN_AFTER", "0")
	check("warn off", 0, time.Hour)

	t.Setenv("MULTICA_AGENT_SILENCE_WARN_AFTER", "2h")
	check("alert below warn clamps", 2*time.Hour, 2*time.Hour)

	t.Setenv("MULTICA_AGENT_SILENCE_WARN_AFTER", "")
	t.Setenv("MULTICA_AGENT_SILENCE_ALERT_AFTER", "0")
	check("alert off", 15*time.Minute, 0)

	t.Setenv("MULTICA_AGENT_SILENCE_ALERT_AFTER", "nonsense")
	if _, err := LoadConfig(Overrides{ServerURL: "http://localhost:8080", WorkspacesRoot: t.TempDir()}); err == nil {
		t.Fatal("invalid value: want error like the other duration envs")
	}
}

func TestClientReportSilenceAlertPostsSegment(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, body = r.URL.Path, string(b)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := NewClient(srv.URL).ReportSilenceAlert(context.Background(), "t1", "seg9", 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if path != "/api/daemon/tasks/t1/silence-alert" || !strings.Contains(body, `"segment_id":"seg9"`) || !strings.Contains(body, `"silent_for_seconds":90`) {
		t.Fatalf("path=%s body=%s", path, body)
	}
}

// Cursor's tool count refreshes lastActivityAt as a side effect. The stamp must
// be read after that call, or a tool that just ended reads as long silence.
func TestSilenceReadsActivityAfterToolCountRefresh(t *testing.T) {
	r := newSilenceRig(15*time.Minute, time.Hour)
	r.now = time.Hour
	r.w.inFlight = func() int32 {
		r.message() // native tool activity ended just now
		return 0
	}
	r.w.step()
	r.want(t, 0, 0)
}

// A marker post can block; if the run moves on meanwhile, the alert must not
// fire from the old snapshot.
func TestSilenceAlertSkippedWhenRunMovesDuringMarker(t *testing.T) {
	for name, move := range map[string]func(r *silenceRig){
		"message":    func(r *silenceRig) { r.message() },
		"tool start": func(r *silenceRig) { r.tools = 1 },
	} {
		r := newSilenceRig(15*time.Minute, time.Hour)
		r.w.mark = func(typ, content, callID, tool string) {
			r.marks = append(r.marks, typ)
			move(r)
		}
		r.advance(time.Hour)
		if len(r.alerts) != 0 {
			t.Fatalf("%s: alert fired from a stale snapshot: %v", name, r.alerts)
		}
	}
}
