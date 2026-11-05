package execenv

import "strings"

// CodexProjectDocDefaultBytes is Codex's built-in project_doc_max_bytes. Codex
// silently drops everything past it when it loads AGENTS.md (CHE-3687).
const CodexProjectDocDefaultBytes = 32 * 1024

// codexProjectDocHeadroom leaves room for a repository's own AGENTS.md, which
// Codex concatenates under the same cap.
const codexProjectDocHeadroom = 16 * 1024

// CodexProjectDocLimit returns the project_doc_max_bytes the daemon passes to
// Codex for an AGENTS.md of fileBytes: never below Codex's default.
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
	TotalBytes int            `json:"total_bytes"`
	LimitBytes int            `json:"limit_bytes"`
	Truncated  bool           `json:"truncated"`
	Sections   []BriefSection `json:"sections"`
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
