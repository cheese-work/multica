package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2269: github_review_members and github_review_authors are patched as
// top-level fields, validated against the workspace, and leave other keys alone.
func TestWorkspaceGitHubReviewMappingFields(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	workspace, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, workspace.Settings, testWorkspaceID)
	})
	dbfx.Exec(t, `UPDATE workspace SET settings = '{"compact_prompt":"Keep SHAs."}' WHERE id = $1`, testWorkspaceID)
	agentID := createHandlerTestAgent(t, "review-author", nil)

	patch := func(body map[string]any, want int) map[string]any {
		t.Helper()
		request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, body), "id", testWorkspaceID)
		testutil.Call(t, testHandler.UpdateWorkspace, request).Want(want)
		stored, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
		if err != nil {
			t.Fatal(err)
		}
		var settings map[string]any
		if err := json.Unmarshal(stored.Settings, &settings); err != nil {
			t.Fatal(err)
		}
		return settings
	}

	members := map[string]any{"4242": testUserID}
	authors := map[string]any{"acme/widgets#7": agentID}
	settings := patch(map[string]any{"github_review_members": members, "github_review_authors": authors}, http.StatusOK)
	if !reflect.DeepEqual(settings["github_review_members"], members) || !reflect.DeepEqual(settings["github_review_authors"], authors) {
		t.Fatalf("mappings not saved: %v", settings)
	}
	if settings["compact_prompt"] != "Keep SHAs." {
		t.Errorf("other settings keys were lost: %v", settings)
	}

	// A patch naming one map leaves the other as saved.
	settings = patch(map[string]any{"github_review_members": map[string]any{}}, http.StatusOK)
	if !reflect.DeepEqual(settings["github_review_members"], map[string]any{}) || !reflect.DeepEqual(settings["github_review_authors"], authors) {
		t.Fatalf("one-map patch changed the other: %v", settings)
	}

	// Rejections leave the saved value as it was.
	missing := "00000000-0000-4000-8000-000000000000"
	for name, body := range map[string]map[string]any{
		"non-numeric github user": {"github_review_members": map[string]any{"octocat": testUserID}},
		"unknown member":          {"github_review_members": map[string]any{"4242": missing}},
		"not a uuid":              {"github_review_members": map[string]any{"4242": "me"}},
		"bad pull key":            {"github_review_authors": map[string]any{"acme/widgets": agentID}},
		"unknown agent":           {"github_review_authors": map[string]any{"acme/widgets#7": missing}},
		"not an object":           {"github_review_members": []string{testUserID}},
	} {
		if got := patch(body, http.StatusBadRequest); !reflect.DeepEqual(got["github_review_authors"], authors) {
			t.Errorf("%s: saved authors changed to %v", name, got["github_review_authors"])
		}
	}
}
