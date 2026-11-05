package execenv

import (
	"strings"
	"testing"
)

// CHE-4100: an output_only background task gets a read-only brief with no
// multica CLI instructions.
func TestBuildMetaSkillContentBackgroundIsReadOnly(t *testing.T) {
	got := buildMetaSkillContent("claude", TaskContextForEnv{BackgroundJob: "dm_title", AgentName: "Eve", AgentID: "eve-1"})
	if !strings.Contains(strings.ToLower(got), "read-only") {
		t.Fatalf("brief must say read-only:\n%s", got)
	}
	if strings.Contains(got, "multica issue") || strings.Contains(got, "multica ") {
		t.Fatalf("brief must not teach the multica CLI:\n%s", got)
	}
}
