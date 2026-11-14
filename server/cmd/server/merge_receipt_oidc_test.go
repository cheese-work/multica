package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/handler"
)

// CHE-5594: the CI receipt route sits outside the Multica auth chain (the OIDC
// token is its credential) and is off until a valid policy is configured.
func TestMergeReceiptCIRouteIsPublicAndOffByDefault(t *testing.T) {
	if testServer == nil {
		t.Skip("no database connection")
	}
	req, err := http.NewRequest(http.MethodPut, testServer.URL+"/api/github/merge-receipts/ci", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unconfigured CI receipt route: status %d, want 404 (not 401 from Auth)", resp.StatusCode)
	}
}

func TestConfigureMergeReceiptOIDCFailsClosed(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yml")
	good := filepath.Join(dir, "good.yml")
	os.WriteFile(bad, []byte("entries: []\n"), 0o600)
	os.WriteFile(good, []byte(`entries:
  - repository: cheese-work/multica
    repository_id: "1"
    repository_owner_id: "2"
    workflow: .github/workflows/merge-receipt.yml
    events: [schedule]
    ref: refs/heads/main
    runner_environment: github-hosted
    environments: [""]
    permissions: {contents: read}
`), 0o600)
	for name, env := range map[string][2]string{
		"no policy":      {"", "ws"},
		"no workspace":   {good, ""},
		"unreadable":     {filepath.Join(dir, "missing.yml"), "ws"},
		"invalid policy": {bad, "ws"},
	} {
		t.Setenv("MULTICA_MERGE_RECEIPT_OIDC_POLICY", env[0])
		t.Setenv("MULTICA_MERGE_RECEIPT_WORKSPACE_ID", env[1])
		h := &handler.Handler{}
		configureMergeReceiptOIDC(h)
		if h.MergeReceiptBroker != nil {
			t.Fatalf("%s: route turned on, want off", name)
		}
	}
	t.Setenv("MULTICA_MERGE_RECEIPT_OIDC_POLICY", good)
	t.Setenv("MULTICA_MERGE_RECEIPT_WORKSPACE_ID", "ws")
	h := &handler.Handler{}
	configureMergeReceiptOIDC(h)
	if h.MergeReceiptBroker == nil || h.MergeReceiptBroker.Verifier.Audience != handler.MergeReceiptAudience {
		t.Fatalf("valid policy: broker %v, want one with the receipt audience", h.MergeReceiptBroker)
	}
}
