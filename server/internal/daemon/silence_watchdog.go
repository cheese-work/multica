package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// toolCall is one tool call that has started and not yet returned.
type toolCall struct {
	id, tool string
	since    time.Duration // awake-clock reading when the call started
}

// toolCallRegistry tracks running tool calls by transcript call id.
type toolCallRegistry struct {
	mu sync.Mutex
	m  map[string]toolCall
}

func (r *toolCallRegistry) start(id, tool string, since time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]toolCall{}
	}
	r.m[id] = toolCall{id: id, tool: tool, since: since}
}

func (r *toolCallRegistry) finish(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, id)
}

func (r *toolCallRegistry) running() []toolCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]toolCall, 0, len(r.m))
	for _, c := range r.m {
		out = append(out, c)
	}
	return out
}

// silenceWatchdog marks a silent run on the transcript (warn) and reports it to
// the issue (alert). It never stops the run.
type silenceWatchdog struct {
	warn, alert time.Duration
	last        *atomic.Int64
	inFlight    func() int32
	calls       func() []toolCall
	now         func() time.Duration
	mark        func(typ, content, callID, tool string)
	raise       func(segmentID string, silentFor time.Duration) error
	log         *slog.Logger

	segment int64
	warned  bool
	alerted bool
	stalled map[string]bool
}

// step is one tick. A silent segment starts at each lastActivityAt value, so a
// new message (which moves the stamp) re-arms both tiers. While a tool is in
// flight the run is not silent; only a call past the warn threshold is marked,
// once per call.
func (w *silenceWatchdog) step() {
	// inFlight first: a backend's tool count may refresh the activity stamp.
	toolRunning := w.inFlight() > 0
	last := w.last.Load()
	if last != w.segment {
		w.segment, w.warned, w.alerted = last, false, false
	}
	now := w.now()
	if toolRunning {
		if w.warn <= 0 || w.calls == nil {
			return
		}
		for _, c := range w.calls() {
			if w.stalled[c.id] || now-c.since < w.warn {
				continue
			}
			if w.stalled == nil {
				w.stalled = map[string]bool{}
			}
			w.stalled[c.id] = true
			w.mark("tool_stalled", fmt.Sprintf("Tool call running for %s with no result", (now-c.since).Round(time.Second)), c.id, c.tool)
		}
		return
	}
	silent := now - time.Duration(last)
	if w.warn > 0 && !w.warned && silent >= w.warn {
		w.warned = true
		w.mark("silence_warn", fmt.Sprintf("No agent activity for %s", silent.Round(time.Second)), "", "")
		// The marker post can block; do not alert from a snapshot the run has left.
		if w.inFlight() > 0 || w.last.Load() != last {
			return
		}
	}
	if w.alert > 0 && !w.alerted && silent >= w.alert {
		w.alerted = true
		if err := w.raise(strconv.FormatInt(last, 10), silent); err != nil {
			w.log.Warn("silence alert not delivered", "error", err)
		}
	}
}

// run ticks until ctx ends. The tick follows the smaller enabled tier.
func (w *silenceWatchdog) run(ctx context.Context) {
	tier := w.warn
	if tier <= 0 || (w.alert > 0 && w.alert < tier) {
		tier = w.alert
	}
	ticker := time.NewTicker(idleWatchdogTickInterval(tier))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.step()
		}
	}
}

// resolveSilenceTiers raises alert to warn when both tiers are on and alert is
// lower, so the alert never precedes the marker. clamped reports the change.
func resolveSilenceTiers(warn, alert time.Duration) (time.Duration, time.Duration, bool) {
	if warn > 0 && alert > 0 && alert < warn {
		return warn, warn, true
	}
	return warn, alert, false
}
