package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	publicapiv1 "github.com/multica-ai/multica/server/pkg/publicapi/v1"
	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

// A guard hook decides whether a write may happen. If its own callback token
// could write, the verdict could be bought by changing the thing being judged
// from inside the call. So a token minted for a guard call is read-only on every
// route, whatever scopes the installation holds.
func guardCallbackToken(t *testing.T, installationID, issueID string) string {
	t.Helper()
	installation, err := testHandler.PluginService.InstallationForWorkspace(
		context.Background(), parseUUID(testWorkspaceID), installationID)
	if err != nil {
		t.Fatalf("load installation: %v", err)
	}
	token, err := testHandler.PluginService.Callbacks.Issue(context.Background(), service.HookInvocation{
		Installation: installation,
		Hook:         plugincontract.Hook{Key: "summarize"},
		Trigger:      plugincontract.TriggerGuard,
		Actor:        service.HookActor{Type: "member", ID: parseUUID(testUserID)},
		IssueID:      parseUUID(issueID),
	})
	if err != nil {
		t.Fatalf("issue guard callback token: %v", err)
	}
	return token
}

func TestGuardCallbackTokenRefusesEveryWriteRoute(t *testing.T) {
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	withCallbackTokens(t)
	installationID := installHookPlugin(t)
	issueID := createTestIssue(t, "Guard callback is read-only", "todo", "none")
	token := guardCallbackToken(t, installationID, issueID)

	issueParams := map[string]string{"issue_ref": issueID}
	storageParams := map[string]string{"scope": "workspace", "key": "k"}
	writes := []struct {
		method, path string
		handler      http.HandlerFunc
		body         any
		params       map[string]string
	}{
		{http.MethodPatch, publicapiv1.PathIssue, testHandler.PatchPluginIssue, map[string]any{"title": "hijacked"}, issueParams},
		{http.MethodPost, publicapiv1.PathIssueComments, testHandler.CreatePluginComment, map[string]any{"content": "should never land"}, issueParams},
		{http.MethodPut, publicapiv1.PathStorageValue, testHandler.PutPluginStorage, map[string]any{"value": "v"}, storageParams},
		{http.MethodDelete, publicapiv1.PathStorageValue, testHandler.DeletePluginStorage, nil, storageParams},
		{http.MethodPost, "/api/plugin-bridge/v1/hooks/summarize", testHandler.InvokePluginHook, map[string]any{"trigger": "manual"}, map[string]string{"key": "summarize"}},
	}

	// Drift guard: a new mutating Action API operation must be listed above, or
	// this test fails and says which one was added.
	covered := map[string]bool{}
	for _, write := range writes {
		covered[write.method+" "+write.path] = true
	}
	for _, operation := range publicapiv1.Operations {
		if operation.Method == http.MethodGet || operation.Method == http.MethodHead {
			continue
		}
		if !covered[operation.Method+" "+operation.Path] {
			t.Errorf("%s %s is a mutating Action API operation not covered by the guard read-only test", operation.Method, operation.Path)
		}
	}

	for _, write := range writes {
		recorder := httptest.NewRecorder()
		write.handler(recorder, callbackRequest(token, write.method, write.path, write.body, write.params))
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "guard_read_only") {
			t.Errorf("%s %s: status=%d body=%s, want 403 guard_read_only", write.method, write.path, recorder.Code, recorder.Body.String())
		}
	}
	var comments int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID).Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if comments != 0 {
		t.Fatalf("a refused guard write left %d comment(s) behind", comments)
	}

	// Reads still work: the guard needs the issue to decide.
	read := httptest.NewRecorder()
	testHandler.GetPluginIssue(read, callbackRequest(token, http.MethodGet, "/v1/issues/"+issueID, nil, issueParams))
	if read.Code != http.StatusOK {
		t.Fatalf("a guard token must still read: status=%d body=%s", read.Code, read.Body.String())
	}
}
