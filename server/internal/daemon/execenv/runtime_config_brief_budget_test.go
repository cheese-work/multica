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
}

const issueBriefTotalCap = 18000

func TestIssueBriefStaysInsideByteBudget(t *testing.T) {
	t.Parallel()
	brief := buildMetaSkillContent("claude", TaskContextForEnv{IssueID: "issue-1", AgentID: "a-1", AgentName: "Eve",
		AgentSkills: []SkillContextForEnv{{Name: platformSkillName, BuiltinName: platformSkillName}}})
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
		AgentSkills: []SkillContextForEnv{{Name: platformSkillName, BuiltinName: platformSkillName}}}
	brief := buildMetaSkillContent("claude", ctx)
	if !strings.Contains(brief, "references/issue-turn.md") {
		t.Errorf("workflow lost the pointer to the moved procedure\n---\n%s", brief)
	}
}
