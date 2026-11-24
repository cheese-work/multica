package oomwatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Entry is one journald JSON entry, reduced to the fields the reader trusts.
type Entry struct {
	Cursor     string
	BootID     string
	Transport  string
	Identifier string
	UID        string
	Message    string
	SourceMono string // _SOURCE_MONOTONIC_TIMESTAMP (9b distinct-key rule)
	Mono       string // __MONOTONIC_TIMESTAMP
}

// ParseEntry decodes one line of `journalctl --output=json`. A field that is
// present but neither a string nor a byte array (a repeated field, a field
// journalctl printed as null) is an error: the reader must not guess.
func ParseEntry(line []byte) (Entry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return Entry{}, err
	}
	if raw == nil {
		return Entry{}, errors.New("journal entry is not an object")
	}
	var e Entry
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"__CURSOR", &e.Cursor},
		{"_BOOT_ID", &e.BootID},
		{"_TRANSPORT", &e.Transport},
		{"SYSLOG_IDENTIFIER", &e.Identifier},
		{"_UID", &e.UID},
		{"MESSAGE", &e.Message},
		{"_SOURCE_MONOTONIC_TIMESTAMP", &e.SourceMono},
		{"__MONOTONIC_TIMESTAMP", &e.Mono},
	} {
		v, ok := raw[f.key]
		if !ok {
			continue
		}
		s, err := fieldString(v)
		if err != nil {
			return Entry{}, fmt.Errorf("field %s: %w", f.key, err)
		}
		*f.dst = s
	}
	return e, nil
}

// fieldString reads a JSON string, or the byte array journald uses for a value
// that is not valid UTF-8.
func fieldString(v json.RawMessage) (string, error) {
	v = bytes.TrimSpace(v)
	if len(v) == 0 {
		return "", errors.New("empty value")
	}
	switch v[0] {
	case '"':
		var s string
		err := json.Unmarshal(v, &s)
		return s, err
	case '[':
		var nums []int
		if err := json.Unmarshal(v, &nums); err != nil {
			return "", errors.New("not a string or byte array")
		}
		b := make([]byte, len(nums))
		for i, n := range nums {
			if n < 0 || n > 255 {
				return "", errors.New("byte out of range")
			}
			b[i] = byte(n)
		}
		return string(b), nil
	}
	return "", errors.New("not a string or byte array")
}

// Cursor is a parsed journal cursor (s=;i=;b=;m=;t=;x=).
type Cursor struct {
	SeqnumID string
	Seqnum   uint64
	Boot     string
	Mono     uint64
}

// ParseCursor parses a journald cursor string. The s, i, b and m parts are
// required; a repeated or unknown part is an error.
func ParseCursor(s string) (Cursor, error) {
	var c Cursor
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || seen[k] {
			return Cursor{}, fmt.Errorf("bad cursor %q", s)
		}
		seen[k] = true
		var err error
		switch k {
		case "s":
			c.SeqnumID = v
		case "b":
			c.Boot = v
		case "i":
			c.Seqnum, err = strconv.ParseUint(v, 16, 64)
		case "m":
			c.Mono, err = strconv.ParseUint(v, 16, 64)
		case "t", "x":
			_, err = strconv.ParseUint(v, 16, 64)
		default:
			err = errors.New("unknown part")
		}
		if err != nil {
			return Cursor{}, fmt.Errorf("bad cursor %q: part %s: %w", s, k, err)
		}
	}
	for _, k := range []string{"s", "i", "b", "m"} {
		if !seen[k] {
			return Cursor{}, fmt.Errorf("bad cursor %q: missing %s", s, k)
		}
	}
	if c.SeqnumID == "" || c.Boot == "" {
		return Cursor{}, fmt.Errorf("bad cursor %q: empty part", s)
	}
	return c, nil
}

// AtOrBefore reports whether entry cursor a is at or before cursor b in the
// same boot. Cursors of different boots are never ordered (false, false).
// Within one journal file (same seqnum id) the sequence number orders them;
// across files of one boot the monotonic time does.
func AtOrBefore(a, b Cursor) (before bool, comparable bool) {
	switch {
	case a.Boot != b.Boot:
		return false, false
	case a.SeqnumID == b.SeqnumID:
		return a.Seqnum <= b.Seqnum, true
	}
	return a.Mono <= b.Mono, true
}
