package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	progressInterval = 100 * time.Millisecond // at most 10 redraws or callbacks per second
	progressBarWidth = 20
)

// Progress draws a download bar and phase lines on w, only when w is a
// terminal. A nil *Progress is silent, so quiet callers (the daemon) pass nothing.
type Progress struct {
	// OnUpdate, if set, is called with the byte counts at most 10 times per
	// second and once more on Finish with the final counts. Set it before use.
	OnUpdate func(done, total int64)
	// OnPhase, if set, is called with each Phase name. Set it before use.
	OnPhase func(name string)

	w     io.Writer
	tty   bool
	now   func() time.Time
	start time.Time

	mu       sync.Mutex
	done     int64
	total    int64
	last     time.Time // last throttled draw/callback
	open     bool      // a bar line is on screen without its newline
	notified int64     // done value last delivered to OnUpdate; -1 = never
}

// NewProgress reports on w. Output is on only when w is a terminal file.
func NewProgress(w io.Writer) *Progress {
	f, ok := w.(*os.File)
	tty := ok && isTerminal(f)
	return &Progress{w: w, tty: tty, now: time.Now, start: time.Now(), notified: -1}
}

// Wrap counts bytes read from r against total (0 = unknown size).
func (p *Progress) Wrap(r io.Reader, total int64) io.Reader {
	if p == nil {
		return r
	}
	p.mu.Lock()
	p.start, p.total = p.now(), total
	p.mu.Unlock()
	return &progressReader{r: r, p: p, total: total}
}

type progressReader struct {
	r     io.Reader
	p     *Progress
	n     int64
	total int64
}

func (pr *progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	pr.n += int64(n)
	pr.p.Set(pr.n, pr.total)
	return n, err
}

// Set records a byte count supplied from outside (a poller) and redraws if
// the throttle allows.
func (p *Progress) Set(done, total int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done, p.total = done, total
	if now := p.now(); now.Sub(p.last) >= progressInterval {
		p.last = now
		p.drawLocked(now)
		p.notifyLocked()
	}
}

// Phase ends any open bar line, then prints name on its own line.
func (p *Progress) Phase(name string) {
	if p == nil {
		return
	}
	p.Finish()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty {
		fmt.Fprintln(p.w, name)
	}
	if p.OnPhase != nil {
		p.OnPhase(name)
	}
}

// Finish draws the final counts, ends the bar line and delivers the final
// callback. Safe to call repeatedly, after success or error.
func (p *Progress) Finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open {
		p.drawLocked(p.now())
		fmt.Fprintln(p.w)
		p.open = false
	}
	p.notifyLocked()
}

func (p *Progress) notifyLocked() {
	if p.OnUpdate != nil && p.notified != p.done {
		p.notified = p.done
		p.OnUpdate(p.done, p.total)
	}
}

func (p *Progress) drawLocked(now time.Time) {
	if !p.tty {
		return
	}
	const mb = 1 << 20
	done := float64(p.done) / mb
	var line string
	if p.total > 0 {
		pct := int(min(p.done*100/p.total, 100))
		fill := pct * progressBarWidth / 100
		bar := strings.Repeat("=", fill)
		if fill < progressBarWidth {
			bar += ">" + strings.Repeat(" ", progressBarWidth-fill-1)
		}
		line = fmt.Sprintf("[%s] %3d%%  %.1f / %.1f MB", bar, pct, done, float64(p.total)/mb)
	} else {
		line = fmt.Sprintf("%.1f MB", done)
		if secs := now.Sub(p.start).Seconds(); secs > 0 {
			line += fmt.Sprintf("  %.1f MB/s", done/secs)
		}
	}
	fmt.Fprintf(p.w, "\r\x1b[K%s", line)
	p.open = true
}
