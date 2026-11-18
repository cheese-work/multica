package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckoutRepositoryAllowlistBeforeCache(t *testing.T) {
	const repo = "https://github.com/tevo-studio/sa041-downloader-5.git"
	cache := &recordingRepoCache{lookupPath: "/cache/denied.git"}
	dir := t.TempDir()
	d := newRepoCheckoutTestDaemon(t, "ws-checkout", repo, dir, cache)
	d.workspaces["ws-checkout"].settings = []byte(`{"guard_policy":{"version":1,"plugins":[],"repository_allowlist":{"tevo-studio":["sa039-astrology"]}}}`)
	body := strings.NewReader(`{"url":"` + repo + `","workspace_id":"ws-checkout","workdir":"` + dir + `","task_id":"task-1"}`)
	rec := httptest.NewRecorder()
	d.repoCheckoutHandler().ServeHTTP(rec, authorizedRepoCheckoutRequest(body))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "allowlist") {
		t.Fatalf("denied checkout reached cache: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
