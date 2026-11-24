package oomwatch

import (
	"bufio"
	"fmt"
	"io"
	"sync"
	"time"
)

// maxLine bounds one journal JSON line (a kernel message is a few hundred
// bytes; this is a safety cap, not a tuning value).
const maxLine = 16 << 20

// ReaderConfig configures a Reader.
type ReaderConfig struct {
	Boot   string // 32-hex journald form
	UID    string // uid of the daemon, "1000" on X99
	Cursor string // last processed cursor (history boundary); may be empty
	// ResumeFrom is the last persisted heartbeat time of a same-boot resume; zero
	// when the reader did not resume. The first heartbeat the reader sees decides
	// whether the interval was a gap.
	ResumeFrom time.Time
	Classifier Config
	Monitor    *HeartbeatMonitor
	Sinks      Sinks
}

// Reader processes entries of the production filter. Process and Tick may run
// on different goroutines. Sinks run under the reader's lock and must not call
// back into it.
type Reader struct {
	mu       sync.Mutex
	cfg      ReaderConfig
	cls      *Classifier
	boundary *Cursor // entries at or before it are history
	cursor   string
	lastBeat time.Time // when the reader last saw its own heartbeat
	resume   time.Time // last persisted heartbeat, until the first new one is seen
}

// NewReader returns a reader. An unparsable starting cursor is not a history
// boundary: the reader then processes everything it is given (fail closed).
func NewReader(cfg ReaderConfig) *Reader {
	if cfg.Monitor == nil {
		cfg.Monitor = NewHeartbeatMonitor()
	}
	r := &Reader{cfg: cfg, cls: NewClassifier(cfg.Classifier), cursor: cfg.Cursor, resume: cfg.ResumeFrom}
	if c, err := ParseCursor(cfg.Cursor); err == nil {
		r.boundary = &c
	}
	return r
}

// Process handles one journal entry.
func (r *Reader) Process(e Entry, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e.BootID != r.cfg.Boot {
		r.ignore("other_boot", e)
		return
	}
	ec, err := ParseCursor(e.Cursor)
	if err != nil {
		r.hold(HoldUnparsable, "entry cursor: "+err.Error(), now)
		return
	}
	if r.boundary != nil {
		if before, ok := AtOrBefore(ec, *r.boundary); ok && before {
			r.ignore("history", e)
			return
		}
	}
	r.cursor = e.Cursor

	switch {
	case e.Transport == "kernel":
		if v, ok := r.cls.Feed(e.Message); ok {
			r.verdict(v, now)
		}
	case e.Identifier == HeartbeatTag && e.UID == r.cfg.UID:
		nonce, ok := ParseMessage(e.Message)
		switch {
		case !ok:
			r.ignore("heartbeat_unparsable", e)
		case !r.cfg.Monitor.Seen(nonce, now):
			r.ignore("heartbeat_unknown_or_late_nonce", e)
		default:
			r.firstHeartbeat(now)
			r.lastBeat = now
		}
	default:
		r.ignore("other_source", e)
	}
}

// Tick checks heartbeat deadlines at the given time. A nonce that was not seen
// within its window latches the hold and opens a telemetry gap.
func (r *Reader) Tick(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.cfg.Monitor.Expired(now) {
		r.hold(HoldHeartbeatMissed, "nonce "+n+" not seen within "+HeartbeatWindow.String(), now)
		if g := r.cfg.Sinks.Gap; g != nil {
			g(GapEvent{Reason: GapHeartbeatMissed, From: r.lastBeat, To: now})
		}
	}
}

// Cursor returns the cursor of the last entry processed. It never moves for
// history or for another boot.
func (r *Reader) Cursor() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor
}

// Run reads JSON lines until the stream ends or fails. A line that is not a
// journal entry latches the hold and reading goes on: the line may have held
// the constraint line or the record.
func (r *Reader) Run(src io.Reader, now func() time.Time) error {
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		e, err := ParseEntry(line)
		if err != nil {
			r.mu.Lock()
			r.hold(HoldUnparsable, "journal line: "+err.Error(), now())
			r.mu.Unlock()
			continue
		}
		r.Process(e, now())
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read journal stream: %w", err)
	}
	return nil
}

// firstHeartbeat closes the resume boundary: the interval from the last
// persisted heartbeat to the first new one is a gap when it exceeds
// resumeGapAfter. Later heartbeats are not measured.
func (r *Reader) firstHeartbeat(now time.Time) {
	from := r.resume
	if from.IsZero() {
		return
	}
	r.resume = time.Time{}
	if now.Sub(from) > resumeGapAfter {
		if g := r.cfg.Sinks.Gap; g != nil {
			g(GapEvent{Reason: GapResume, From: from, To: now})
		}
	}
}

func (r *Reader) verdict(v Verdict, now time.Time) {
	if s := r.cfg.Sinks.Verdict; s != nil {
		s(v)
	}
	if v.Hold {
		r.hold(v.Cause, v.Line, now)
	}
}

func (r *Reader) hold(cause HoldCause, detail string, now time.Time) {
	if s := r.cfg.Sinks.Hold; s != nil {
		s(HoldEvent{Cause: cause, Detail: detail, At: now})
	}
}

func (r *Reader) ignore(reason string, e Entry) {
	if s := r.cfg.Sinks.Ignored; s != nil {
		s(reason, e)
	}
}
