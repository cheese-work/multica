package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type revUpload struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	// RevisionOf is the id of the previous revision, nil for the first.
	RevisionOf *string `json:"revision_of"`
}

// uploadRev posts one file to /api/upload-file for an issue, optionally as a
// replacement of an earlier attachment.
func uploadRev(t *testing.T, issueID, filename, content, replaces string) (*testutil.Response, revUpload) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte(content))
	if issueID != "" {
		mw.WriteField("issue_id", issueID)
	}
	if replaces != "" {
		mw.WriteField("replaces", replaces)
	}
	mw.Close()
	req := httptest.NewRequest("POST", "/api/upload-file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	resp := testutil.Call(t, testHandler.UploadFile, req)
	var out revUpload
	if resp.Code == http.StatusOK {
		if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode upload: %v", err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, out.ID)
		})
	}
	return resp, out
}

func withMockStorage(t *testing.T) {
	t.Helper()
	orig := testHandler.Storage
	testHandler.Storage = &mockStorage{}
	t.Cleanup(func() { testHandler.Storage = orig })
}

func attachmentGet(t *testing.T, fn http.HandlerFunc, id, path string) *testutil.Response {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	return testutil.Call(t, fn, withURLParam(req, "id", id))
}

func TestAttachmentReplacesLinksRevisions(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "replaces chain", nil)
	_, v1 := uploadRev(t, issueID, "plan.md", "a\n", "")
	if v1.Revision != 1 || v1.RevisionOf != nil {
		t.Fatalf("first upload = %+v, want revision 1, no revision_of", v1)
	}
	// Different file name: only --replaces links it.
	resp, v2 := uploadRev(t, issueID, "plan-final.md", "b\n", v1.ID)
	resp.Want(http.StatusOK)
	if v2.Revision != 2 || v2.RevisionOf == nil || *v2.RevisionOf != v1.ID {
		t.Fatalf("replaced upload = %+v, want revision 2 of %s", v2, v1.ID)
	}
	// Replacing an older revision continues from the head, never forks.
	_, v3 := uploadRev(t, issueID, "plan.md", "c\n", v1.ID)
	if v3.Revision != 3 || v3.RevisionOf == nil || *v3.RevisionOf != v2.ID {
		t.Fatalf("replace of v1 = %+v, want revision 3 of head %s", v3, v2.ID)
	}
}

func TestAttachmentSameNameOnIssueChains(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "same name chain", nil)
	_, v1 := uploadRev(t, issueID, "design.html", "<p>1</p>\n", "")
	_, v2 := uploadRev(t, issueID, "design.html", "<p>2</p>\n", "")
	if v2.Revision != 2 || v2.RevisionOf == nil || *v2.RevisionOf != v1.ID {
		t.Fatalf("same-name upload = %+v, want revision 2 of %s", v2, v1.ID)
	}
	// Another issue with the same name starts its own chain.
	other := dbfx.Issue(t, "same name other issue", nil)
	_, o1 := uploadRev(t, other, "design.html", "x\n", "")
	if o1.Revision != 1 || o1.RevisionOf != nil {
		t.Fatalf("other issue upload = %+v, want a fresh chain", o1)
	}
}

func TestAttachmentReplacesRejectsOtherIssueAndWorkspace(t *testing.T) {
	withMockStorage(t)
	issueA := dbfx.Issue(t, "replaces A", nil)
	issueB := dbfx.Issue(t, "replaces B", nil)
	_, a1 := uploadRev(t, issueA, "a.md", "a\n", "")
	// Same workspace, different issue: refused.
	resp, _ := uploadRev(t, issueB, "a.md", "b\n", a1.ID)
	resp.Want(http.StatusBadRequest)
	// Unknown id (what a foreign-workspace id looks like to this workspace): refused.
	resp, _ = uploadRev(t, issueA, "a.md", "b\n", "00000000-0000-0000-0000-000000000099")
	resp.Want(http.StatusBadRequest)
	// replaces without an issue: refused.
	resp, _ = uploadRev(t, "", "a.md", "b\n", a1.ID)
	resp.Want(http.StatusBadRequest)
}

func TestAttachmentHistoryAndTextDiff(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "history diff", nil)
	_, v1 := uploadRev(t, issueID, "notes.md", "alpha\nbeta\n", "")
	_, v2 := uploadRev(t, issueID, "notes.md", "alpha\ngamma\n", "")

	// History reads the same from any revision of the chain.
	w := attachmentGet(t, testHandler.ListAttachmentRevisions, v1.ID, "/api/attachments/"+v1.ID+"/history").Want(http.StatusOK)
	var hist struct {
		Revisions []struct {
			ID       string `json:"id"`
			Revision int    `json:"revision"`
			SHA256   string `json:"sha256"`
			Actor    string `json:"uploader_id"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Revisions) != 2 || hist.Revisions[0].ID != v1.ID || hist.Revisions[1].ID != v2.ID {
		t.Fatalf("history = %+v, want [v1 v2]", hist.Revisions)
	}
	if len(hist.Revisions[0].SHA256) != 64 || hist.Revisions[0].SHA256 == hist.Revisions[1].SHA256 {
		t.Fatalf("sha256 missing or equal: %+v", hist.Revisions)
	}

	w = attachmentGet(t, testHandler.DiffAttachmentRevisions, v2.ID, "/api/attachments/"+v2.ID+"/diff?from=1&to=2").Want(http.StatusOK)
	var diff struct {
		Binary bool   `json:"binary"`
		Diff   string `json:"diff"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if diff.Binary || !strings.Contains(diff.Diff, "-beta") || !strings.Contains(diff.Diff, "+gamma") {
		t.Fatalf("diff = %+v, want -beta/+gamma", diff)
	}
	attachmentGet(t, testHandler.DiffAttachmentRevisions, v2.ID, "/api/attachments/"+v2.ID+"/diff?from=1&to=9").Want(http.StatusNotFound)
}

func TestAttachmentBinaryDiffFallsBack(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "binary diff", nil)
	_, v1 := uploadRev(t, issueID, "pic.png", "\x89PNG\r\n\x1a\none", "")
	_, v2 := uploadRev(t, issueID, "pic.png", "\x89PNG\r\n\x1a\ntwo!", "")
	w := attachmentGet(t, testHandler.DiffAttachmentRevisions, v1.ID, "/api/attachments/"+v1.ID+"/diff?from=1&to=2").Want(http.StatusOK)
	var diff struct {
		Binary  bool   `json:"binary"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if !diff.Binary || !strings.Contains(diff.Message, "binary, sizes and SHA differ") {
		t.Fatalf("diff = %+v, want binary fallback", diff)
	}
	_ = v2
}

func TestAttachmentRevisionEndpointsStayInWorkspace(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "boundary", nil)
	_, v1 := uploadRev(t, issueID, "n.md", "a\n", "")
	req := httptest.NewRequest("GET", "/api/attachments/"+v1.ID+"/history", nil)
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", "00000000-0000-0000-0000-000000000099")
	// Another workspace never resolves the attachment.
	testutil.Call(t, testHandler.ListAttachmentRevisions, withURLParam(req, "id", v1.ID)).Want(http.StatusNotFound)
}

func TestAttachmentDiffFullKeepsEveryUnchangedLine(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "full diff", nil)
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "line"+strings.Repeat("x", i))
	}
	old := strings.Join(lines, "\n") + "\n"
	lines[10] = "changed"
	_, v1 := uploadRev(t, issueID, "long.md", old, "")
	uploadRev(t, issueID, "long.md", strings.Join(lines, "\n")+"\n", "")

	get := func(q string) string {
		w := attachmentGet(t, testHandler.DiffAttachmentRevisions, v1.ID, "/api/attachments/"+v1.ID+"/diff?from=1&to=2"+q).Want(http.StatusOK)
		var d struct {
			Diff string `json:"diff"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d.Diff
	}
	if folded := get(""); strings.Contains(folded, "line"+strings.Repeat("x", 0)+"\n") {
		t.Fatalf("default diff should fold far-away lines:\n%s", folded)
	}
	full := get("&full=1")
	if !strings.Contains(full, " line\n") || !strings.Contains(full, " line"+strings.Repeat("x", 19)+"\n") {
		t.Fatalf("full diff should keep first and last lines:\n%s", full)
	}
}

// Deleting revision 1 must not split the chain: a later upload keeps the
// original root, so history from any surviving revision lists every one.
func TestAttachmentChainSurvivesDeletedFirstRevision(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "chain after delete", nil)
	_, v1 := uploadRev(t, issueID, "plan.md", "a\n", "")
	_, v2 := uploadRev(t, issueID, "plan.md", "b\n", "")
	if _, err := testPool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, v1.ID); err != nil {
		t.Fatal(err)
	}
	_, v3 := uploadRev(t, issueID, "plan.md", "c\n", "")
	if v3.Revision != 3 || v3.RevisionOf == nil || *v3.RevisionOf != v2.ID {
		t.Fatalf("upload after delete = %+v, want revision 3 of %s", v3, v2.ID)
	}
	w := attachmentGet(t, testHandler.ListAttachmentRevisions, v2.ID, "/api/attachments/"+v2.ID+"/history").Want(http.StatusOK)
	var hist struct {
		Revisions []struct {
			ID string `json:"id"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Revisions) != 2 || hist.Revisions[0].ID != v2.ID || hist.Revisions[1].ID != v3.ID {
		t.Fatalf("history from v2 = %+v, want [v2 v3]", hist.Revisions)
	}
}
