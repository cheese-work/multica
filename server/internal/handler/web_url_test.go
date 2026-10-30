package handler

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2572: CLI-visible responses that name a platform object carry an
// absolute web_url built from the server's configured app origin. The relative
// url field stays as it was for the web app.

const webURLTestOrigin = "https://app.example.test"

func setTestAppURL(t *testing.T, appURL string) {
	t.Helper()
	previous := testHandler.cfg.AppURL
	testHandler.cfg.AppURL = appURL
	t.Cleanup(func() { testHandler.cfg.AppURL = previous })
}

func decodeObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return object
}

func wantWebURL(t *testing.T, object map[string]any, want string) {
	t.Helper()
	got, present := object["web_url"]
	if want == "" {
		if present {
			t.Fatalf("web_url = %v, want it omitted", got)
		}
		return
	}
	if got != want {
		t.Fatalf("web_url = %v, want %q (body %v)", got, want, object)
	}
}

func TestChatContinueWebURL(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for name, appURL := range map[string]string{"configured": webURLTestOrigin + "/", "unconfigured": ""} {
		t.Run(name, func(t *testing.T) {
			setTestAppURL(t, appURL)
			agentID := createHandlerTestAgent(t, "web-url-continue-"+name, []byte("[]"))
			sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{})
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sourceID, "role": "user", "content": "Goal."})
			response := continueChatRequest(t, sourceID, "Retained goal.", "")
			if response.Code != http.StatusCreated {
				t.Fatalf("continue: %d %s", response.Code, response.Body.String())
			}
			object := decodeObject(t, response.Body.Bytes())
			childID := object["session"].(map[string]any)["id"].(string)
			dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, childID)
			dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, childID)
			relative := "/" + handlerTestWorkspaceSlug + "/chat?session=" + childID
			if object["url"] != relative {
				t.Fatalf("url = %v, want unchanged %q", object["url"], relative)
			}
			if appURL == "" {
				wantWebURL(t, object, "")
			} else {
				wantWebURL(t, object, webURLTestOrigin+relative)
			}
		})
	}
}

func TestChatForkWebURL(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setTestAppURL(t, webURLTestOrigin)
	agentID := createHandlerTestAgent(t, "web-url-fork", []byte("[]"))
	sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{})
	forkMessage(t, sourceID, testutil.Cols{"role": "user", "content": "Fork me"})
	response := httptest.NewRecorder()
	testHandler.ForkChatSession(response, forkChatRequest(t, sourceID, "web-url-fork-key"))
	if response.Code != http.StatusCreated {
		t.Fatalf("fork: %d %s", response.Code, response.Body.String())
	}
	object := decodeObject(t, response.Body.Bytes())
	childID := object["session"].(map[string]any)["id"].(string)
	cleanupChatFork(t, childID)
	relative := "/" + handlerTestWorkspaceSlug + "/chat?session=" + childID
	if object["url"] != relative {
		t.Fatalf("url = %v, want unchanged %q", object["url"], relative)
	}
	wantWebURL(t, object, webURLTestOrigin+relative)
}

func TestIssueResponsesWebURL(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for name, appURL := range map[string]string{"configured": webURLTestOrigin, "unconfigured": ""} {
		t.Run(name, func(t *testing.T) {
			setTestAppURL(t, appURL)
			create := httptest.NewRecorder()
			testHandler.CreateIssue(create, newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{"title": "web_url " + name}))
			if create.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", create.Code, create.Body.String())
			}
			created := decodeObject(t, create.Body.Bytes())
			issueID := created["id"].(string)
			t.Cleanup(func() { deleteTestIssue(t, issueID) })
			want := ""
			if appURL != "" {
				want = webURLTestOrigin + "/" + handlerTestWorkspaceSlug + "/issues/" + created["identifier"].(string)
			}
			wantWebURL(t, created, want)

			get := httptest.NewRecorder()
			testHandler.GetIssue(get, withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID, nil), "id", issueID))
			if get.Code != http.StatusOK {
				t.Fatalf("get: %d %s", get.Code, get.Body.String())
			}
			wantWebURL(t, decodeObject(t, get.Body.Bytes()), want)

			comment := httptest.NewRecorder()
			testHandler.CreateComment(comment, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": "link me"}), "id", issueID))
			if comment.Code != http.StatusCreated {
				t.Fatalf("comment: %d %s", comment.Code, comment.Body.String())
			}
			commentObject := decodeObject(t, comment.Body.Bytes())
			commentWant := ""
			if appURL != "" {
				commentWant = want + "#comment-" + commentObject["id"].(string)
			}
			wantWebURL(t, commentObject, commentWant)
		})
	}
}

func TestUploadFileWebURL(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	original := testHandler.Storage
	testHandler.Storage = &mockStorage{}
	t.Cleanup(func() { testHandler.Storage = original })
	for name, appURL := range map[string]string{"configured": webURLTestOrigin, "unconfigured": ""} {
		t.Run(name, func(t *testing.T) {
			setTestAppURL(t, appURL)
			agentID := createHandlerTestAgent(t, "web-url-upload-"+name, []byte("[]"))
			sessionID := createHandlerTestChatSession(t, agentID)
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("file", "note.png")
			if err != nil {
				t.Fatal(err)
			}
			part.Write([]byte("\x89PNG\r\n\x1a\nrest-of-bytes"))
			writer.WriteField("chat_session_id", sessionID)
			writer.Close()
			req := httptest.NewRequest(http.MethodPost, "/api/upload-file", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("X-User-ID", testUserID)
			req.Header.Set("X-Workspace-ID", testWorkspaceID)
			response := httptest.NewRecorder()
			testHandler.UploadFile(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("upload: %d %s", response.Code, response.Body.String())
			}
			object := decodeObject(t, response.Body.Bytes())
			attachmentID := object["id"].(string)
			t.Cleanup(func() { testPool.Exec(t.Context(), `DELETE FROM attachment WHERE id = $1`, attachmentID) })
			want := ""
			if appURL != "" {
				want = webURLTestOrigin + "/" + handlerTestWorkspaceSlug + "/attachments/" + attachmentID + "/preview"
			}
			wantWebURL(t, object, want)
		})
	}
}
