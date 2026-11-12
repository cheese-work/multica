package ciwatch

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	errorLine  = regexp.MustCompile(`(?i)##\[error\]|\berror\b|\bfail(ed|ure)?\b|\bpanic\b|\bexception\b|\bfatal\b|cannot |denied|exceeded`)
	stackFrame = regexp.MustCompile(`\.(go|tsx?|jsx?|kt|java|py|rs):\d+|^\s+at \S+|File ".*", line \d+`)

	noiseRes = []struct {
		re   *regexp.Regexp
		with string
	}{
		{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?Z?`), ""},
		{regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`), "<uuid>"},
		{regexp.MustCompile(`(/home/runner/work|/tmp|/var/folders|/private/tmp|/Users/runner)[^\s:]*`), "<path>"},
		{regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`), "<sha>"},
		{regexp.MustCompile(`\d+`), "N"},
	}
)

func normalize(line string) string {
	for _, n := range noiseRes {
		line = n.re.ReplaceAllString(line, n.with)
	}
	return strings.Join(strings.Fields(line), " ")
}

// Signature hashes the first error-like line and the last stack frame of the
// redacted text, normalized (no timestamps, SHAs, UUIDs, numbers, temp
// paths). Text-less evidence falls back to the given names, so a failure with
// no log still groups by workflow and job.
func Signature(redacted string, fallback ...string) string {
	var first, frame string
	for _, l := range strings.Split(redacted, "\n") {
		if first == "" && errorLine.MatchString(l) {
			first = normalize(l)
		}
		if stackFrame.MatchString(l) {
			frame = normalize(l)
		}
	}
	key := "text|" + first + "|" + frame
	if first == "" && frame == "" {
		key = "names|" + strings.Join(fallback, "|")
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
