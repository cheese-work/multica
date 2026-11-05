package execenv

import (
	"strings"
	"testing"
)

// realisticCodexCtx approximates a production Cheese Work issue task: the
// workspace context, project description and agent instructions are what push
// the Codex brief past 32 KiB (CHE-3687).
func realisticCodexCtx() TaskContextForEnv {
	filler := func(n int) string { return strings.Repeat("Rule line with enough words to look like prose.\n", n) }
	return TaskContextForEnv{
		IssueID:            "issue-1",
		AgentID:            "a-1",
		AgentName:          "Eve",
		WorkspaceContext:   filler(300),
		ProjectDescription: filler(40),
		AgentInstructions:  filler(100),
	}
}

func TestCodexBriefFitsProjectDocLimit(t *testing.T) {
	t.Parallel()
	brief := buildMetaSkillContent("codex", realisticCodexCtx())
	if len(brief) <= CodexProjectDocDefaultBytes {
		t.Fatalf("fixture too small (%d bytes) to exercise the cap", len(brief))
	}
	if limit := CodexProjectDocLimit(len(brief)); len(brief) > limit {
		t.Fatalf("Codex would cut the brief: %d bytes > project_doc_max_bytes %d", len(brief), limit)
	}
}

func TestCodexProjectDocLimitKeepsDefaultForSmallBriefs(t *testing.T) {
	t.Parallel()
	if got := CodexProjectDocLimit(1000); got != CodexProjectDocDefaultBytes {
		t.Fatalf("limit = %d, want default %d", got, CodexProjectDocDefaultBytes)
	}
}

func TestBriefReceipt(t *testing.T) {
	t.Parallel()
	brief := "# Title\nintro\n## A\naaaa\n```\n## not a heading\n```\n## B\nbb\n"
	r := NewBriefReceipt(brief, 10)
	if r.TotalBytes != len(brief) || !r.Truncated || r.LimitBytes != 10 {
		t.Fatalf("receipt = %+v", r)
	}
	var names []string
	sum := 0
	for _, s := range r.Sections {
		names = append(names, s.Name)
		sum += s.Bytes
	}
	if got := strings.Join(names, "|"); got != "preamble|A|B" {
		t.Fatalf("sections = %q", got)
	}
	if sum != len(brief) {
		t.Fatalf("section bytes %d != brief %d", sum, len(brief))
	}
	if NewBriefReceipt(brief, len(brief)).Truncated {
		t.Fatal("brief exactly at the limit must not be truncated")
	}
}
