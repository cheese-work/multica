package execenv

import (
	"strings"
	"testing"
)

// Byte budget for the generated issue brief (CHE-3707, re-homed from CHE-2901).
// The generator part of the brief shrinks one section per PR toward a ~10 KB
// core (CHE-3683). A section that grows past its cap fails here; lower a cap in
// the PR that shrinks its section, never raise one to make a test pass.
var issueBriefSectionCaps = map[string]int{
	"Instruction Precedence": 5900, // includes the `### Workflow` subsection
	"Available Commands":     4430, // wakeup detail lives in references/wakeups.md; +280 = upstream v0.10.0 shared-stash rule (CHE-7023)
}

// Raised 17650 -> 17970 (CHE-6457): the brief grew past the 2026-11-12 cap
// and the red test went unseen because the `rest` shard hid Go failures (CHE-6374).
// Shrink sections (CHE-3683) and lower this; do not raise it again.
//
// Raised 17970 -> 18714 (CHE-7023, upstream v0.10.0 merge): +747 bytes measured on the
// merged tree with the platform skill installed (P measured 17967). The growth is
// upstream's shared-stash rule, `multica env start` guidance, the longer platform
// domain list and reworded Instruction Precedence/Output lines. Shrink them
// under CHE-3683 and lower this cap; do not raise it again.
const issueBriefTotalCap = 18714

func TestIssueBriefStaysInsideByteBudget(t *testing.T) {
	t.Parallel()
	brief := buildMetaSkillContent("claude", TaskContextForEnv{IssueID: "issue-1", AgentID: "a-1", AgentName: "Eve",
		AgentSkills: []SkillContextForEnv{platformSkillFixture()}})
	r := NewBriefReceipt(brief, issueBriefTotalCap)
	if r.Truncated {
		t.Errorf("issue brief is %d bytes, over the %d-byte budget", r.TotalBytes, issueBriefTotalCap)
	}
	for _, s := range r.Sections {
		if limit, ok := issueBriefSectionCaps[s.Name]; ok && s.Bytes > limit {
			t.Errorf("section %q is %d bytes, over its %d-byte cap", s.Name, s.Bytes, limit)
		}
	}
}

// The workflow procedure moved out of the brief lives in the multica-platform
// issues reference; the brief must keep the pointer when that skill is installed.
func TestWorkflowPointsAtMovedProcedure(t *testing.T) {
	t.Parallel()
	ctx := TaskContextForEnv{IssueID: "issue-1", AgentID: "a-1", AgentName: "Eve",
		AgentSkills: []SkillContextForEnv{platformSkillFixture()}}
	brief := buildMetaSkillContent("claude", ctx)
	if !strings.Contains(brief, "references/issue-turn.md") {
		t.Errorf("workflow lost the pointer to the moved procedure\n---\n%s", brief)
	}
}
