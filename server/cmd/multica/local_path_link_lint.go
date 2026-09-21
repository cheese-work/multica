package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/markdownlinks"
)

// Agent deliverables must never carry a link to the runtime's own filesystem
// (MUL-4899). The runtime brief now states that contract, but a prompt is
// advisory; this is the enforcement backstop on the three commands that publish
// agent-authored markdown to a human reader.
//
// Scope is deliberately narrow on three axes, because a false positive here
// blocks a legitimate deliverable outright:
//
//  1. Agent task context only. A human running the CLI with a PAT is not
//     linting-eligible: their links are their own business, and they may well
//     have a shared path the reader really can open.
//  2. Real markdown link/image destinations only (see markdownlinks.Find), so
//     a path inside a code span or fenced block — the normal way an agent quotes
//     a path it is *discussing* — is structurally invisible here. A full-text
//     scan cannot make that distinction and would fail exactly the comments that
//     explain this bug.
//  3. High-confidence targets only. See classifyLocalPathTarget.

// localPathLinkFinding is one markdown link/image destination that resolves to
// this machine's filesystem. Reason states the evidence, so the error message
// can tell the agent WHY its link was rejected rather than just that it was.
type localPathLinkFinding struct {
	Target string
	Reason string
}

// findLocalPathLinks returns every link, image, or autolink destination in
// body that is high-confidence a runtime-local path.
//
// Extraction is markdownlinks.Find, shared with the CHE-408 source-link guard;
// only the classification below is specific to this lint. Findings are
// de-duplicated by target so a path linked five times reports once.
//
// Quoted destinations are deliberately NOT exempt: a file:// URL no reader can
// open is just as dead inside a blockquote as outside one.
func findLocalPathLinks(body string) []localPathLinkFinding {
	var findings []localPathLinkFinding
	for _, ref := range markdownlinks.Find(body) {
		reason := classifyLocalPathTarget(ref.Destination)
		if reason == "" {
			continue
		}
		findings = append(findings, localPathLinkFinding{Target: ref.Destination, Reason: reason})
	}
	return findings
}

// classifyLocalPathTarget returns the evidence that target is a runtime-local
// path, or "" when it is not — or when we cannot tell.
//
// Only three signals count, all of them positive evidence that the target names
// THIS machine:
//
//  1. A `file://` URL. Unresolvable for any reader by construction.
//  2. An absolute path inside the current working directory — the task workdir,
//     which is per-task and private.
//  3. An absolute path that names a file existing on this machine right now.
//
// Everything else is allowed through, and the omissions are the point. A bare
// `/foo` is a valid origin-relative URI reference (RFC 3986 §4.2) and is exactly
// how `/acme/issues/123` is written, so an absolute-looking target that neither
// lives in the workdir nor exists on disk is indistinguishable from a legitimate
// in-app link and must not be guessed at. Relative targets are left alone for
// the same reason. The prompt contract in the runtime brief — not this lint — is
// what covers the residue.
func classifyLocalPathTarget(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}

	// Order matters: a Windows drive-absolute path ("C:\Users\me\shot.png")
	// parses as scheme "c" under RFC 3986, so the filesystem test has to run
	// before any scheme sniffing or Windows paths would be dismissed as URLs.
	if filepath.IsAbs(target) {
		if within, err := fileWithinWorkingDir(target); err == nil && within {
			return "it is inside this task's working directory"
		}
		if info, err := os.Stat(target); err == nil && !info.IsDir() {
			return "it names a file that exists only on this machine"
		}
		return ""
	}

	if parsed, err := url.Parse(target); err == nil && strings.EqualFold(parsed.Scheme, "file") {
		return "it is a file:// URL"
	}
	return ""
}

// guardLocalPathLinks fails the command when an agent-authored body publishes a
// link to a runtime-local path. Returns nil outside an agent task context.
//
// deliveryHint is the caller's own fix instruction. It is a parameter rather
// than a constant because the right answer differs per command, and one of them
// is a trap: `multica issue update` has no --attachment flag, so a shared
// "pass --attachment" message would send the agent to an argument that does not
// exist and turn one failure into two.
func guardLocalPathLinks(body, field, deliveryHint string) error {
	if !inAgentExecutionContext() {
		return nil
	}
	findings := findLocalPathLinks(body)
	if len(findings) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s links %d runtime-local path(s), which no reader can open:\n", field, len(findings))
	for _, f := range findings {
		fmt.Fprintf(&b, "  - %q — %s\n", f.Target, f.Reason)
	}
	b.WriteString("\nThe path exists only on the machine running you; for everyone else the link is dead. ")
	b.WriteString(deliveryHint)
	b.WriteString("\nTo merely reference a code location, use inline code instead of a link (`path/to/file.ts:42`) — code spans and fenced blocks are not checked.")
	return fmt.Errorf("%s", b.String())
}
