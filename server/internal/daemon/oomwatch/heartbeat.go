package oomwatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	HeartbeatPeriod = 30 * time.Second
	HeartbeatWindow = 10 * time.Second

	messagePrefix = "hb nonce="
)

var nonceHex = regexp.MustCompile(`^[0-9a-f]{32}$`)

// HeartbeatMonitor tracks nonces the daemon wrote and the reader must see.
type HeartbeatMonitor struct {
	mu      sync.Mutex
	pending map[string]time.Time // nonce -> time it was sent
}

// NewHeartbeatMonitor returns an empty monitor.
func NewHeartbeatMonitor() *HeartbeatMonitor {
	return &HeartbeatMonitor{pending: map[string]time.Time{}}
}

// Sent registers a nonce written at the given time. Call it before the write.
func (m *HeartbeatMonitor) Sent(nonce string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[nonce] = at
}

// Seen records that the reader saw a nonce. It reports false for a nonce that
// is unknown, already seen, or seen after its window: a late nonce does not
// undo the missed deadline.
func (m *HeartbeatMonitor) Seen(nonce string, at time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	sent, ok := m.pending[nonce]
	if !ok || at.Sub(sent) >= HeartbeatWindow {
		return false
	}
	delete(m.pending, nonce)
	return true
}

// Expired removes and returns, oldest first, the nonces whose window passed
// unseen.
func (m *HeartbeatMonitor) Expired(now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for n, sent := range m.pending {
		if now.Sub(sent) >= HeartbeatWindow {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if si, sj := m.pending[out[i]], m.pending[out[j]]; !si.Equal(sj) {
			return si.Before(sj)
		}
		return out[i] < out[j]
	})
	for _, n := range out {
		delete(m.pending, n)
	}
	return out
}

// Message is the logger message for a nonce.
func Message(nonce string) string { return messagePrefix + nonce }

// ParseMessage extracts the nonce from a heartbeat message.
func ParseMessage(msg string) (string, bool) {
	n, ok := strings.CutPrefix(msg, messagePrefix)
	if !ok || !nonceHex.MatchString(n) {
		return "", false
	}
	return n, true
}

// NewNonce returns a fresh 128-bit hex nonce.
func NewNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Logger writes one line to the system journal with a tag (logger -t).
type Logger func(ctx context.Context, tag, message string) error

// ExecLogger returns a Logger that runs `<program> -t <tag> -- <message>`
// (program is the path of util-linux logger).
func ExecLogger(program string) Logger {
	return func(ctx context.Context, tag, message string) error {
		if out, err := exec.CommandContext(ctx, program, "-t", tag, "--", message).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", program, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// Beat registers a fresh nonce and writes it. A write error does not remove
// the nonce: it expires unseen and latches the hold (fail closed).
func Beat(ctx context.Context, m *HeartbeatMonitor, log Logger, now time.Time) error {
	nonce, err := NewNonce()
	if err != nil {
		return err
	}
	m.Sent(nonce, now)
	if err := log(ctx, HeartbeatTag, Message(nonce)); err != nil {
		return fmt.Errorf("heartbeat write: %w", err)
	}
	return nil
}
