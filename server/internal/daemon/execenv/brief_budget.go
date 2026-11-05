package execenv

import "strings"

// CodexProjectDocDefaultBytes is Codex's built-in project_doc_max_bytes. Codex
// silently drops everything past it when it loads AGENTS.md (CHE-3687).
const CodexProjectDocDefaultBytes = 32 * 1024

// codexProjectDocHeadroom is slack over the instruction files the daemon can
// see (separators, and a file added between the stat and the Codex launch).
const codexProjectDocHeadroom = 16 * 1024

// CodexProjectDocLimit returns the project_doc_max_bytes the daemon passes to
// Codex for fileBytes of instruction files (the brief plus the parent
// AGENTS.md files Codex concatenates under the same cap): never below Codex's
// default.
func CodexProjectDocLimit(fileBytes int) int {
	return max(CodexProjectDocDefaultBytes, fileBytes+codexProjectDocHeadroom)
}

// BriefSection is the size of one top-level (`## `) section of the brief.
type BriefSection struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// BriefReceipt records how big the brief is and where the bytes go, so later
// brief cuts can be measured. Truncated is true when LimitBytes would cut it.
type BriefReceipt struct {
	TotalBytes int `json:"total_bytes"`
	// ParentBytes is the size of the other AGENTS.md files Codex loads under
	// the same cap; Truncated counts them.
	ParentBytes int            `json:"parent_bytes"`
	LimitBytes  int            `json:"limit_bytes"`
	Truncated   bool           `json:"truncated"`
	Sections    []BriefSection `json:"sections"`
}

// NewBriefReceipt splits brief on `## ` headings outside code fences. Text
// before the first heading is reported as "preamble".
func NewBriefReceipt(brief string, limit int) BriefReceipt {
	r := BriefReceipt{TotalBytes: len(brief), LimitBytes: limit, Truncated: len(brief) > limit}
	name, bytes, fenced := "preamble", 0, false
	flush := func() {
		if bytes > 0 {
			r.Sections = append(r.Sections, BriefSection{Name: name, Bytes: bytes})
		}
	}
	for _, line := range strings.SplitAfter(brief, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "## ") {
			flush()
			name, bytes = strings.TrimSpace(line[3:]), 0
		}
		bytes += len(line)
	}
	flush()
	return r
}
