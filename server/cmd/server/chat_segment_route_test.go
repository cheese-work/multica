package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func getAsTask(t *testing.T, token, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, testServer.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// CHE-2819 L2b: the CLI reads segments without a session id; the task token
// names the DM, like /api/chat/history.
func TestChatSegmentRoutesResolveTheTaskTokensOwnDM(t *testing.T) {
	f := newCompactFixture(t)
	if code, body := getAsTask(t, f.token, "/api/chat/segments"); code != http.StatusOK || !strings.Contains(body, `"segments"`) {
		t.Fatalf("segments = %d %s, want 200 with a segments list", code, body)
	}
	code, body := getAsTask(t, f.token, "/api/chat/segment?divider="+uuid.NewString())
	if code != http.StatusNotFound || !strings.Contains(body, "segment not found") {
		t.Fatalf("unknown divider = %d %s, want 404 segment not found", code, body)
	}
}
