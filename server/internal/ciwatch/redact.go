package ciwatch

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
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
)

var (
	keyBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*KEY[A-Z0-9 ]*-----`)
	keyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*KEY[A-Z0-9 ]*-----`)

	// URL credentials go first so a token inside them is covered as well.
	urlCreds   = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@]+@`)
	tokenShape = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`)
	bearer     = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
)

func redactTokens(s string) string {
	s = urlCreds.ReplaceAllString(s, "${1}"+tokenPlaceholer+"@")
	s = tokenShape.ReplaceAllString(s, tokenPlaceholer)
	return bearer.ReplaceAllString(s, "${1}"+tokenPlaceholer)
}

// redactor is the stateful half of RedactTail: it sees every line from byte 0,
// so inKey is always complete.
type redactor struct {
	maxLines int
	lines    []string
	inKey    bool
}

func (r *redactor) emit(s string) {
	r.lines = append(r.lines, s)
	if len(r.lines) > 2*r.maxLines {
		r.lines = append(r.lines[:0], r.lines[len(r.lines)-r.maxLines:]...)
	}
}

// line takes one logical line. text is what may be stored; first and last are
// the kinds of the first and last key marker anywhere in the full line ('B'
// BEGIN, 'E' END, 0 none). The last marker decides whether a key is still open
// after the line, so "END ... BEGIN" on one line keeps the next body hidden.
func (r *redactor) line(text string, first, last byte) {
	if first == 0 {
		if !r.inKey {
			r.emit(redactTokens(text))
		}
		return
	}
	if !r.inKey {
		if first == 'E' {
			// END without BEGIN: the key started before anything we saw as a
			// key, so nothing earlier is trustworthy. Drop it all.
			r.lines = r.lines[:0]
		}
		r.emit(keyPlaceholder)
	}
	r.inKey = last == 'B'
}

// lineScan accumulates one logical line from ReadSlice fragments.
type lineScan struct {
	head        []byte
	overflow    bool
	first, last byte // kind of the first and last key marker seen; 0 none
	carry       string
}

// span returns the first and last match start in s, or -1, -1.
func span(re *regexp.Regexp, s string) (lo, hi int) {
	m := re.FindAllStringIndex(s, -1)
	if len(m) == 0 {
		return -1, -1
	}
	return m[0][0], m[len(m)-1][0]
}

func (l *lineScan) add(frag []byte) {
	s := l.carry + string(frag)
	// A marker re-found in the carry keeps its kind and order, so first and
	// last stay right when a marker straddles two fragments.
	loB, hiB := span(keyBegin, s)
	loE, hiE := span(keyEnd, s)
	if loB >= 0 || loE >= 0 {
		if l.first == 0 {
			l.first = 'E'
			if loE < 0 || (loB >= 0 && loB < loE) {
				l.first = 'B'
			}
		}
		l.last = 'E'
		if hiE < 0 || hiB > hiE {
			l.last = 'B'
		}
	}
	if len(s) > markerCarry {
		s = s[len(s)-markerCarry:]
	}
	l.carry = s
	if room := maxLineBytes - len(l.head); room > 0 {
		if len(frag) > room {
			frag, l.overflow = frag[:room], true
		}
		l.head = append(l.head, frag...)
	} else {
		l.overflow = true
	}
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
	var cur lineScan
	for {
		frag, rerr := br.ReadSlice('\n')
		if len(frag) > 0 || rerr == nil {
			cur.add(frag)
		}
		if rerr == bufio.ErrBufferFull {
			continue
		}
		if rerr == nil || (errors.Is(rerr, io.EOF) && len(cur.head) > 0) {
			text := strings.TrimRight(string(cur.head), "\r\n")
			if cur.overflow {
				text += " [line cut at 64 KiB]"
			}
			red.line(text, cur.first, cur.last)
			cur = lineScan{}
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
