package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// The list endpoint rebuilds each db.Workspace by hand, so a field the
// rebuild skips silently reads as "not paused" in the workspace switcher.
func TestListWorkspacesKeepsSchedulingPauseFields(t *testing.T) {
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET scheduling_paused_at = NULL, scheduling_paused_by = NULL WHERE id = $1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET scheduling_paused_at = NULL, scheduling_paused_by = NULL WHERE id = $1`, testWorkspaceID)
	})
	pausedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET scheduling_paused_at = $1, scheduling_paused_by = $2 WHERE id = $3`, pausedAt, testUserID, testWorkspaceID); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	testHandler.ListWorkspaces(w, newRequest("GET", "/api/workspaces", nil))
	if w.Code != 200 {
		t.Fatalf("ListWorkspaces: %d %s", w.Code, w.Body.String())
	}
	var list []WorkspaceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, ws := range list {
		if ws.ID != testWorkspaceID {
			continue
		}
		if ws.SchedulingPausedBy == nil || *ws.SchedulingPausedBy != testUserID {
			t.Errorf("scheduling_paused_by = %v, want %s", ws.SchedulingPausedBy, testUserID)
		}
		if ws.SchedulingPausedAt == nil {
			t.Fatal("scheduling_paused_at missing from workspace list")
		}
		if got, err := time.Parse(time.RFC3339, *ws.SchedulingPausedAt); err != nil || !got.Equal(pausedAt) {
			t.Errorf("scheduling_paused_at = %s, want %s (err %v)", *ws.SchedulingPausedAt, pausedAt.Format(time.RFC3339), err)
		}
		return
	}
	t.Fatalf("workspace %s missing from list: %s", testWorkspaceID, w.Body.String())
}
