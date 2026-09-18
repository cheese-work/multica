// Package sourcelink answers one question: does this body actually cite the
// source it was required to cite?
//
// The defect it exists for (CHE-153) is an agent writing "read the
// authoritative 01-22 plan" and shipping it. That sentence is not a citation —
// the reader cannot follow it, and the agent has discharged its obligation in
// appearance only. Prose naming a source is what this package rejects; a
// working link to the declared destination is what it accepts.
//
// Deliberately NOT inferred: which source a body ought to cite. The requirement
// is declared out of band, on the issue, through a write path the body's author
// does not control. Guessing the obligation from the prose would let an agent
// escape it by rephrasing, which is the same bypass in a new costume.
package sourcelink

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/multica-ai/multica/server/internal/markdownlinks"
)

// Verdict is the outcome of a check, carrying enough detail for the caller to
// tell the author what to fix. A bare boolean would make the API's failure mode
// "rejected, figure out why" — which is how an agent burns a retry loop.
type Verdict struct {
	OK bool

	// RequiredSource is the declared destination, echoed back so the diagnostic
	// can name it. Empty when nothing was declared.
	RequiredSource string

	// Reason states why the body failed, in a sentence meant for the author.
	// Empty when OK.
	Reason string
}

// Check reports whether body cites requiredSource.
//
// An empty requiredSource passes: no declaration, no obligation. That is the
// ordinary case for every issue that has not opted in, and it must stay cheap.
func Check(body, requiredSource string) Verdict {
	required := strings.TrimSpace(requiredSource)
	if required == "" {
		return Verdict{OK: true}
	}

	want, err := canonical(required)
	if err != nil {
		// The declared source is itself unusable. Failing closed here would
		// deadlock the issue: no body could ever satisfy it, and the author of
		// the body is not who misconfigured it. Say so against the property.
		return Verdict{
			RequiredSource: requiredSource,
			Reason: fmt.Sprintf(
				"the declared required source %q is not a usable http(s) URL, so no content can satisfy it; fix the required-source property on this issue",
				requiredSource),
		}
	}

	for _, ref := range markdownlinks.Find(body) {
		if ref.Quoted {
			// Quoting someone else's citation is not making one.
			continue
		}
		got, err := canonical(ref.Destination)
		if err != nil {
			continue
		}
		if got == want {
			return Verdict{OK: true, RequiredSource: requiredSource}
		}
	}

	return Verdict{
		RequiredSource: requiredSource,
		Reason: fmt.Sprintf(
			"this content must link its required source (%s) as a markdown link, and does not. "+
				"Naming it in prose is not enough — the reader cannot follow a sentence. "+
				"A different URL does not substitute, and a destination inside a code span, a fenced block, or a blockquote does not count as your citation.",
			requiredSource),
	}
}

// canonical reduces a URL to the identity that decides whether two links fetch
// the same thing, and returns an error for anything that cannot be a citation.
//
// Normalized away: scheme and host case (per RFC 3986 both are
// case-insensitive) and the fragment (client-side; `#L10` on a plan is the same
// document, and demanding an exact match would reject a reader-friendlier
// deep link).
//
// Deliberately NOT normalized: path case, and the query string. Path case is
// significant on most servers, and a commit SHA in a path is exactly the
// "full immutable revision" the requirement can pin — folding case there would
// let a different file pass. The query can select a different resource
// entirely.
func canonical(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("empty destination")
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	// http(s) only. A file:// URL is dead for every reader but the author
	// (MUL-4899), a mention:// is an in-app action rather than a source, and a
	// bare relative path cannot be resolved without knowing where it was
	// written. None of them can serve as a citation of an external source.
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("not an http(s) URL")
	}
	if u.Host == "" {
		return "", fmt.Errorf("no host")
	}
	if isPlaceholderHost(u.Host) {
		return "", fmt.Errorf("placeholder host")
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawFragment = ""
	return u.String(), nil
}

// isPlaceholderHost rejects the reserved example domains (RFC 2606). They
// resolve, so a liveness check would pass them, but they are what a model
// writes when it is inventing a plausible URL rather than citing one — the
// "guessed destination" the requirement names.
func isPlaceholderHost(host string) bool {
	h := strings.ToLower(host)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	switch h {
	case "example.com", "example.org", "example.net", "example.edu", "localhost":
		return true
	}
	return strings.HasSuffix(h, ".example") || strings.HasSuffix(h, ".invalid") || strings.HasSuffix(h, ".localhost")
}
