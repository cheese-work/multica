package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const testAttID = "11111111-1111-4111-8111-111111111111"

func attachmentTestServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
}

func TestRunAttachmentHistoryPrintsRevisionsShaAndIct(t *testing.T) {
	attachmentTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/attachments/"+testAttID+"/history" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"revisions": []map[string]any{
			{"revision": 1, "uploader_type": "member", "uploader_id": "u-1", "sha256": "aaaa1111", "created_at": "2026-11-10T17:30:00Z"},
			{"revision": 2, "uploader_type": "agent", "uploader_id": "a-1", "sha256": "bbbb2222", "created_at": "2026-11-11T01:00:00Z"},
		}})
	})
	cmd := &cobra.Command{Use: "history"}
	cmd.Flags().String("output", "table", "")
	out, err := captureStdout(t, func() error { return runAttachmentHistory(cmd, []string{testAttID}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"REVISION", "ACTOR", "SHA-256", "2026-11-11 00:30 ICT", "member:u-1", "agent:a-1", "bbbb2222"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func diffCmd(from, to string) *cobra.Command {
	cmd := &cobra.Command{Use: "diff"}
	cmd.Flags().String("output", "table", "")
	cmd.Flags().Int64("from", 0, "")
	cmd.Flags().Int64("to", 0, "")
	_ = cmd.Flags().Set("from", from)
	_ = cmd.Flags().Set("to", to)
	return cmd
}

func TestRunAttachmentDiffPrintsTextAndBinaryFallback(t *testing.T) {
	var uri string
	body := map[string]any{"binary": false, "diff": "--- v1\n+++ v2\n@@ -1 +1 @@\n-a\n+b\n"}
	attachmentTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		uri = r.URL.RequestURI()
		_ = json.NewEncoder(w).Encode(body)
	})
	out, err := captureStdout(t, func() error { return runAttachmentDiff(diffCmd("1", "2"), []string{testAttID}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "-a\n+b\n") || !strings.Contains(uri, "from=1") || !strings.Contains(uri, "to=2") {
		t.Errorf("text diff wrong; uri=%s out:\n%s", uri, out)
	}

	body = map[string]any{"binary": true, "message": "binary, sizes and SHA differ"}
	out, err = captureStdout(t, func() error { return runAttachmentDiff(diffCmd("1", "2"), []string{testAttID}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "binary, sizes and SHA differ") {
		t.Errorf("binary fallback missing:\n%s", out)
	}
}

func TestRunAttachmentDiffNeedsFromAndTo(t *testing.T) {
	if err := runAttachmentDiff(diffCmd("0", "2"), []string{testAttID}); err == nil || !strings.Contains(err.Error(), "--from") {
		t.Errorf("err = %v, want --from required", err)
	}
}

func TestRunAttachmentUploadSendsIssueAndReplaces(t *testing.T) {
	var gotIssue, gotReplaces, gotFile string
	attachmentTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/issues/"+testIssueUUID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testIssueUUID, "identifier": "MUL-1", "title": "t"})
		case r.URL.Path == "/api/upload-file":
			_ = r.ParseMultipartForm(1 << 20)
			gotIssue, gotReplaces = r.FormValue("issue_id"), r.FormValue("replaces")
			f, h, _ := r.FormFile("file")
			if f != nil {
				gotFile = h.Filename
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "new-id", "revision": 2, "content_type": "text/plain", "markdown_url": "u"})
		default:
			http.NotFound(w, r)
		}
	})
	path := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "upload"}
	cmd.Flags().String("task", "", "")
	cmd.Flags().String("issue", "", "")
	cmd.Flags().String("replaces", "", "")
	_ = cmd.Flags().Set("issue", testIssueUUID)
	_ = cmd.Flags().Set("replaces", testAttID)
	if _, err := captureStdout(t, func() error { return runAttachmentUpload(cmd, []string{path}) }); err != nil {
		t.Fatal(err)
	}
	if gotIssue != testIssueUUID || gotReplaces != testAttID || gotFile != "plan.md" {
		t.Errorf("form = issue %q replaces %q file %q", gotIssue, gotReplaces, gotFile)
	}
}
