package ciwatch

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	sharedredact "github.com/multica-ai/multica/server/pkg/redact"
)

const (
	// maxLineBytes caps one stored line. Longer lines are cut, but every byte
	// is still scanned for key markers (see lineScan).
	maxLineBytes = 64 << 10
	// markerCarry is longer than any key marker, so a marker split across two
	// fragments of an overlong line is still seen.
	markerCarry     = 64
	keyPlaceholder  = "[REDACTED PRIVATE KEY]"
	tokenPlaceholer = "[REDACTED]"
	lineOmitted     = "[line over 64 KiB omitted]"
)

var (
	keyMarker = regexp.MustCompile(`-----(BEGIN|END) ([A-Z0-9 ]*KEY[A-Z0-9 ]*)-----`)

	// URL credentials go first so a token inside them is covered as well.
	urlCreds   = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@]+@`)
	tokenShape = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`)
	bearer     = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
)

func redactTokens(s string) string {
	// URL credentials first: a provider-shaped username replaced by a
	// placeholder with spaces would otherwise break this match and leave the password.
	s = urlCreds.ReplaceAllString(s, "${1}"+tokenPlaceholer+"@")
	s = sharedredact.Text(s)
	s = tokenShape.ReplaceAllString(s, tokenPlaceholer)
	return bearer.ReplaceAllString(s, "${1}"+tokenPlaceholer)
}

// redactor is the stateful half of RedactTail: it sees every line from byte 0,
// so the open-key state is always complete.
type redactor struct {
	maxLines int
	lines    []string
	inKey    bool
	label    string // label of the open BEGIN; only an END with this label closes it
	nested   bool   // a BEGIN arrived inside an open key: fail closed, stay open to EOF
	cred     credMasker
}

func (r *redactor) emit(s string) {
	r.lines = append(r.lines, s)
	if len(r.lines) > 2*r.maxLines {
		r.lines = append(r.lines[:0], r.lines[len(r.lines)-r.maxLines:]...)
	}
}

// line takes one logical line and the scan of its key markers. A line with a
// marker is never stored as text. Without a marker, text is stored only when
// no key is open.
func (r *redactor) line(text string, sc *lineScan) {
	if !sc.sawMarker {
		if !r.inKey {
			if sc.overflow {
				r.cred.omitted(sc.credWord)
			} else {
				text = r.cred.line(text)
			}
			r.emit(redactTokens(text))
		}
		return
	}
	if sc.orphanEnd {
		// END without a matching BEGIN: the key started before anything we saw
		// as a key, so nothing earlier is trustworthy. Drop it all.
		r.lines = r.lines[:0]
		r.emit(keyPlaceholder)
	} else if !r.inKey {
		r.emit(keyPlaceholder)
	}
	r.inKey, r.label, r.nested = sc.open, sc.label, sc.nested
}

// lineScan accumulates one logical line from ReadSlice fragments. It walks the
// key markers in order, starting from the key state before the line, so
// "END a ... BEGIN b" on one line keeps the next body hidden and a mismatched
// END never closes an open key.
type lineScan struct {
	head      []byte
	overflow  bool
	sawMarker bool
	orphanEnd bool   // an END arrived while no key was open
	open      bool   // key state after the markers seen so far
	label     string // label of the open BEGIN
	nested    bool   // see redactor.nested
	carry     string
	credWord  bool   // the line names a credential word anywhere, also past the cap
	wordCarry string // tail of the line, so a split credential word is still seen
	seen      int    // bytes of the line consumed before carry
	done      int    // offset in the line just past the last marker handled
}

func (l *lineScan) add(frag []byte) {
	s := l.carry + string(frag)
	base := l.seen - len(l.carry) // line offset of s[0]
	for _, m := range keyMarker.FindAllStringSubmatchIndex(s, -1) {
		if base+m[0] < l.done {
			continue // already handled while it sat in the carry
		}
		l.done = base + m[1]
		l.sawMarker = true
		label := s[m[4]:m[5]]
		switch {
		case s[m[2]] == 'B':
			// A BEGIN inside an open key makes the END pairing ambiguous: fail closed to EOF.
			if l.open {
				l.nested = true
			} else {
				l.open, l.label = true, label
			}
		case l.open && !l.nested && label == l.label:
			l.open = false
		case !l.open:
			l.orphanEnd = true
		}
	}
	l.seen += len(frag)
	if len(s) > markerCarry {
		s = s[len(s)-markerCarry:]
	}
	l.carry = s
	room := maxLineBytes - len(l.head)
	switch {
	case l.overflow:
		// Keep scanning for credential words, but retain nothing more.
		l.scanCredWord(frag)
		return
	case len(frag) > room:
		// The line just outgrew the cap. It is never parsed from here on, so
		// look for a credential word in everything seen so far.
		l.overflow = true
		l.scanCredWord(append(append([]byte(nil), l.head...), frag...))
		frag = frag[:room]
	}
	l.head = append(l.head, frag...)
}

// scanCredWord looks for a credential word in a line that does not fit the cap:
// credMasker never parses such a line, so this is the only place it is seen.
func (l *lineScan) scanCredWord(frag []byte) {
	if l.credWord {
		return
	}
	w := l.wordCarry + string(frag)
	l.credWord = credWord.MatchString(w)
	l.wordCarry = w[max(0, len(w)-credWordCarry):]
}

// RedactTail streams r through the redactor and returns the last maxLines
// redacted lines. It always reads from byte 0 and holds at most one line plus
// 2*maxLines lines in memory. Any failure returns an error and no lines
// (fail closed); the caller must store no log text then.
func RedactTail(r io.Reader, maxLines int) (lines []string, err error) {
	defer func() {
		if p := recover(); p != nil {
			lines, err = nil, fmt.Errorf("redactor panic: %v", p)
		}
	}()
	red := &redactor{maxLines: maxLines}
	br := bufio.NewReaderSize(r, 32<<10)
	cur := lineScan{}
	for {
		frag, rerr := br.ReadSlice('\n')
		if len(frag) > 0 || rerr == nil {
			cur.add(frag)
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		if rerr == nil || (errors.Is(rerr, io.EOF) && len(cur.head) > 0) {
			text := strings.TrimRight(string(cur.head), "\r\n")
			if cur.overflow {
				// A cut line can hold half a credential (the "@" lies past the cut), so drop the text.
				text = lineOmitted
			}
			red.line(text, &cur)
			cur = lineScan{open: red.inKey, label: red.label, nested: red.nested}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, rerr
		}
	}
	if len(red.lines) > maxLines {
		red.lines = red.lines[len(red.lines)-maxLines:]
	}
	return red.lines, nil
}

// LimitTail keeps whole lines from the end while the joined size stays within
// max bytes. A single line longer than max is cut to its last max bytes.
func LimitTail(lines []string, max int) string {
	size, from := 0, len(lines)
	for from > 0 {
		add := len(lines[from-1])
		if size > 0 {
			add++ // joining newline
		}
		if size+add > max {
			break
		}
		size += add
		from--
	}
	if from == len(lines) && len(lines) > 0 {
		last := lines[len(lines)-1]
		return last[len(last)-max:]
	}
	return strings.Join(lines[from:], "\n")
}
