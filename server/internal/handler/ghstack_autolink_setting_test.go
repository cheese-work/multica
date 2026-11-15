package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The settings write refuses an invalid github_stack_autolink and stores a
// valid one; an absent key stays valid.
func TestGHStackAutolinkSettingValidatedOnWrite(t *testing.T) {
	wsID := pauseWorkspaceFixture(t, "ghstack-autolink-write")
	patch := func(settings map[string]any) *testutil.Response {
		req := withURLParam(newRequest("PATCH", "/api/workspaces/"+wsID, map[string]any{"settings": settings}), "id", wsID)
		return testutil.Call(t, testHandler.UpdateWorkspace, req)
	}
	patch(map[string]any{"github_stack_autolink": map[string]any{"mode": "strict"}}).Want(http.StatusBadRequest)
	patch(map[string]any{"github_stack_autolink": map[string]any{"repos": map[string]any{"acme": "link"}}}).Want(http.StatusBadRequest)
	patch(map[string]any{"github_stack_autolink": nil}).Want(http.StatusBadRequest)
	patch(map[string]any{}).Want(http.StatusOK)
	patch(map[string]any{"github_stack_autolink": map[string]any{"mode": "report", "repos": map[string]any{"acme/app": "link"}}}).Want(http.StatusOK)
}
