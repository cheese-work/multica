package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func withArtifactStorage(test *testing.T) *mockStorage {
	test.Helper()
	store := &mockStorage{}
	original := testHandler.Storage
	testHandler.Storage = store
	test.Cleanup(func() { testHandler.Storage = original })
	return store
}

func sha256Of(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// seedArtifact stores body and attaches it to issueID in the test workspace.
func seedArtifact(test *testing.T, store *mockStorage, issueID, workspaceID, filename, body string) string {
	test.Helper()
	url, err := store.Upload(context.Background(), "artifacts/"+issueID+"/"+filename, []byte(body), "text/markdown", filename)
	if err != nil {
		test.Fatal(err)
	}
	return dbfx.Insert(test, "attachment", testutil.Cols{
		"workspace_id": workspaceID, "issue_id": issueID, "uploader_type": "member", "uploader_id": testUserID,
		"filename": filename, "url": url, "content_type": "text/markdown", "size_bytes": len(body),
	})
}

func (fixture requestCoreFixture) artifactRequest(ask decisiontx.Ask) *http.Request {
	request := newRequest(http.MethodPost, "/api/decisions", map[string]any{
		"issue_id": fixture.issueID, "session_id": fixture.sessionID, "ask": ask, "escalation_class": "reversible",
	})
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	return request
}

func (fixture requestCoreFixture) artifactAsk(artifacts ...decisiontx.Artifact) decisiontx.Ask {
	ask := fixture.structuredAsk()
	ask.Subject, ask.Artifacts = decisiontx.SubjectArtifactApproval, artifacts
	return ask
}

func TestDecisionRequestArtifactRefusals(test *testing.T) {
	store := withArtifactStorage(test)
	const missing = "55555555-5555-4555-8555-555555555555"
	const body = "# Design\nrev 5\n"
	cases := []struct {
		name string
		ask  func(fixture requestCoreFixture, good string) decisiontx.Ask
		want string
	}{
		{"hash mismatch", func(f requestCoreFixture, good string) decisiontx.Ask {
			return f.artifactAsk(decisiontx.Artifact{AttachmentID: good, Label: "rev 5", SHA256: sha256Of("something else")})
		}, "ask.artifacts[0].sha256 does not match the stored file"},
		{"missing attachment", func(f requestCoreFixture, _ string) decisiontx.Ask {
			return f.artifactAsk(decisiontx.Artifact{AttachmentID: missing, Label: "rev 5", SHA256: sha256Of(body)})
		}, "ask.artifacts[0].attachment_id is not an attachment in this workspace"},
		{"other workspace", func(f requestCoreFixture, _ string) decisiontx.Ask {
			otherWorkspace := dbfx.Workspace(test, "Artifact other workspace", "artifact-other-"+f.issueID)
			foreign := dbfx.Insert(test, "attachment", testutil.Cols{
				"workspace_id": otherWorkspace, "issue_id": f.issueID, "uploader_type": "member", "uploader_id": testUserID,
				"filename": "foreign.md", "url": "https://cdn.example.com/foreign", "content_type": "text/markdown", "size_bytes": 1,
			})
			return f.artifactAsk(decisiontx.Artifact{AttachmentID: foreign, Label: "rev 5", SHA256: sha256Of(body)})
		}, "ask.artifacts[0].attachment_id is not an attachment in this workspace"},
		{"other issue", func(f requestCoreFixture, _ string) decisiontx.Ask {
			elsewhere := seedArtifact(test, store, dbfx.Issue(test, "Other issue for artifact"), uuidToString(f.issue.WorkspaceID), "other.md", "other\n")
			return f.artifactAsk(decisiontx.Artifact{AttachmentID: elsewhere, Label: "rev 5", SHA256: sha256Of("other\n")})
		}, "ask.artifacts[0].attachment_id is not attached to the source issue"},
		{"missing superseded", func(f requestCoreFixture, good string) decisiontx.Ask {
			return f.artifactAsk(decisiontx.Artifact{AttachmentID: good, Label: "rev 5", SHA256: sha256Of(body), SupersedesAttachmentID: missing})
		}, "ask.artifacts[0].supersedes_attachment_id is not an attachment in this workspace"},
		{"artifact_approval without artifacts", func(f requestCoreFixture, _ string) decisiontx.Ask {
			return f.artifactAsk()
		}, "ask.artifacts requires at least one artifact when subject is artifact_approval"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
			good := seedArtifact(test, store, fixture.issueID, uuidToString(fixture.issue.WorkspaceID), "design-rev5.md", body)
			response := testutil.Call(test, testHandler.RequestDecision, fixture.artifactRequest(tc.ask(fixture, good))).Want(http.StatusBadRequest)
			if !strings.Contains(response.Body.String(), tc.want) {
				test.Fatalf("body = %s, want %q", response.Body.String(), tc.want)
			}
			fixture.assertCounts(test, 0)
		})
	}
}

func TestDecisionRequestArtifactsStoredAndListed(test *testing.T) {
	store := withArtifactStorage(test)
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	workspaceID := uuidToString(fixture.issue.WorkspaceID)
	before := "# Plan\n\n" + strings.Repeat("same line\n", 12) + "old ending\n"
	after := "# Plan\n\n" + strings.Repeat("same line\n", 12) + "new ending\n"
	previous := seedArtifact(test, store, fixture.issueID, workspaceID, "plan-rev4.md", before)
	current := seedArtifact(test, store, fixture.issueID, workspaceID, "plan-rev5.md", after)
	ask := fixture.artifactAsk(decisiontx.Artifact{
		AttachmentID: current, Label: "Plan rev 5", SHA256: sha256Of(after), SupersedesAttachmentID: previous,
		SignedBy: &decisiontx.ArtifactSignature{Reviewer: "x99-codex-sol", CommentID: fixture.tagID},
	})
	var created map[string]any
	testutil.Call(test, testHandler.RequestDecision, fixture.artifactRequest(ask)).Want(http.StatusCreated).JSON(&created)
	requestID := created["id"].(string)

	var listed struct {
		Artifacts []struct {
			AttachmentID string `json:"attachment_id"`
			Label        string `json:"label"`
			SHA256       string `json:"sha256"`
			SignedBy     struct {
				Reviewer string `json:"reviewer"`
			} `json:"signed_by"`
			Attachment struct {
				Filename    string `json:"filename"`
				SizeBytes   int64  `json:"size_bytes"`
				DownloadURL string `json:"download_url"`
			} `json:"attachment"`
			Diff struct {
				PreviousFilename string                `json:"previous_filename"`
				Hunks            []decisiontx.DiffHunk `json:"hunks"`
			} `json:"diff"`
		} `json:"artifacts"`
	}
	request := withURLParam(newRequest(http.MethodGet, "/api/decisions/"+requestID+"/artifacts", nil), "id", requestID)
	testutil.Call(test, testHandler.ListDecisionArtifacts, request).Want(http.StatusOK).JSON(&listed)
	if len(listed.Artifacts) != 1 {
		test.Fatalf("artifacts = %+v", listed)
	}
	got := listed.Artifacts[0]
	if got.AttachmentID != current || got.Label != "Plan rev 5" || got.SHA256 != sha256Of(after) || got.SignedBy.Reviewer != "x99-codex-sol" {
		test.Fatalf("wrong artifact: %+v", got)
	}
	if got.Attachment.Filename != "plan-rev5.md" || got.Attachment.SizeBytes != int64(len(after)) || got.Attachment.DownloadURL == "" {
		test.Fatalf("wrong file card: %+v", got.Attachment)
	}
	if got.Diff.PreviousFilename != "plan-rev4.md" || len(got.Diff.Hunks) != 1 {
		test.Fatalf("wrong diff: %+v", got.Diff)
	}
	lines := strings.Join(got.Diff.Hunks[0].Lines, "|")
	if !strings.Contains(lines, "-old ending|+new ending") || strings.Contains(lines, "# Plan") {
		test.Fatalf("diff must hold only the changed hunk, got %q", lines)
	}
}

func TestDecisionRequestWithoutArtifactsBehavesAsToday(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	var created map[string]any
	testutil.Call(test, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusCreated).JSON(&created)
	requestID := created["id"].(string)
	var listed struct {
		Artifacts []any `json:"artifacts"`
	}
	request := withURLParam(newRequest(http.MethodGet, "/api/decisions/"+requestID+"/artifacts", nil), "id", requestID)
	response := testutil.Call(test, testHandler.ListDecisionArtifacts, request).Want(http.StatusOK)
	response.JSON(&listed)
	if listed.Artifacts == nil || len(listed.Artifacts) != 0 {
		test.Fatalf("want an empty artifacts list, got %s", response.Body.String())
	}
}

func TestDecisionArtifactsReadAuthorization(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	requestID := uuidToString(result.Request.ID)
	outsider := dbfx.User(test, "Artifact outsider", "artifact-outsider@example.test")
	request := withURLParam(newRequest(http.MethodGet, "/api/decisions/"+requestID+"/artifacts", nil), "id", requestID)
	request.Header.Set("X-User-ID", outsider)
	testutil.Call(test, testHandler.ListDecisionArtifacts, request).WantOneOf(http.StatusForbidden, http.StatusNotFound, http.StatusUnauthorized)
}
