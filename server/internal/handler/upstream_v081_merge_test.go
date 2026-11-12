package handler

import (
	"encoding/json"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5319 merge preservation (upstream v0.8.1 into the fork). Where both
// sides changed a feature the fork wins: the workspace payload keeps the
// scheduling-pause fields next to upstream's pinned_at, and an owning squad
// leader keeps the done / in_review choice rather than upstream's in_review
// only wording.

func TestWorkspaceResponseKeepsSchedulingPauseNextToPin(t *testing.T) {
	raw, err := json.Marshal(testHandler.workspaceToResponse(db.Workspace{}))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"scheduling_paused_at", "scheduling_paused_by"} {
		if _, ok := got[key]; !ok {
			t.Errorf("workspace payload lost %q: %s", key, raw)
		}
	}
	var typed WorkspaceResponse
	pinned := "2026-01-02T03:04:05Z"
	typed.PinnedAt = &pinned
	raw, _ = json.Marshal(typed)
	if !strings.Contains(string(raw), `"pinned_at":"`+pinned+`"`) {
		t.Errorf("workspace payload lost upstream pinned_at: %s", raw)
	}
}

func TestOwningSquadLeaderKeepsDoneOutcome(t *testing.T) {
	owned := squadOperatingProtocolFor(true, leaderNoWorkflow)
	for _, want := range []string{"multica issue status <issue-id> done", "multica issue status <issue-id> in_review"} {
		if !strings.Contains(owned, want) {
			t.Errorf("owning leader protocol lost %q", want)
		}
	}
}
