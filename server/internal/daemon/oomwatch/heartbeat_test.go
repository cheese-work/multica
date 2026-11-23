package oomwatch

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"
)

func TestHeartbeatMessageRoundTrip(t *testing.T) {
	n, err := NewNonce()
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(n) {
		t.Fatalf("nonce %q, %v", n, err)
	}
	n2, _ := NewNonce()
	if n == n2 {
		t.Fatal("nonces must be fresh")
	}
	got, ok := ParseMessage(Message(n))
	if !ok || got != n {
		t.Fatalf("round trip: %q %v", got, ok)
	}
	for _, bad := range []string{"", "hb", "hb nonce=", "hb nonce=XYZ", "hb nonce=" + n + " extra", "x hb nonce=" + n} {
		if _, ok := ParseMessage(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestHeartbeatMonitorWindow(t *testing.T) {
	m := NewHeartbeatMonitor()
	m.Sent("a", t0)
	if got := m.Expired(t0.Add(HeartbeatWindow - time.Nanosecond)); len(got) != 0 {
		t.Fatalf("expired early: %v", got)
	}
	if got := m.Expired(t0.Add(HeartbeatWindow)); len(got) != 1 || got[0] != "a" {
		t.Fatalf("want a to expire at 10 s, got %v", got)
	}
	if got := m.Expired(t0.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("an expired nonce is reported once, got %v", got)
	}
}

func TestHeartbeatMonitorSeen(t *testing.T) {
	m := NewHeartbeatMonitor()
	m.Sent("a", t0)
	if !m.Seen("a", t0.Add(time.Second)) {
		t.Fatal("known nonce must be accepted")
	}
	if m.Seen("a", t0.Add(2*time.Second)) {
		t.Fatal("a nonce counts once")
	}
	if m.Seen("never-sent", t0) {
		t.Fatal("unknown nonce must be refused")
	}
	if got := m.Expired(t0.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("a seen nonce never expires: %v", got)
	}
}

func TestBeatRegistersBeforeWrite(t *testing.T) {
	m := NewHeartbeatMonitor()
	var tag, msg string
	log := func(_ context.Context, tg, ms string) error {
		tag, msg = tg, ms
		n, ok := ParseMessage(ms)
		if !ok {
			t.Fatalf("message %q", ms)
		}
		// the nonce must already be registered when the write happens
		if !m.Seen(n, t0) {
			t.Fatal("nonce was not registered before the write")
		}
		return nil
	}
	if err := Beat(context.Background(), m, log, t0); err != nil {
		t.Fatal(err)
	}
	if tag != "multica-containment-hb" || msg == "" {
		t.Fatalf("tag %q msg %q", tag, msg)
	}
}

func TestBeatWriteErrorKeepsTheNonce(t *testing.T) {
	m := NewHeartbeatMonitor()
	boom := errors.New("logger failed")
	err := Beat(context.Background(), m, func(context.Context, string, string) error { return boom }, t0)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if got := m.Expired(t0.Add(HeartbeatWindow)); len(got) != 1 {
		t.Fatalf("an unwritten nonce must expire unseen, got %v", got)
	}
}
