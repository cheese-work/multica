package handler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func buildMergedPRWebhookBody(issueIdentifier string, prNumber int, repoOwner, repoName string, installationID int64) map[string]any {
	return map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":           prNumber,
			"html_url":         "https://github.com/" + repoOwner + "/" + repoName + "/pull/" + strconv.Itoa(prNumber),
			"title":            issueIdentifier + ": ship it",
			"body":             "",
			"state":            "closed",
			"draft":            false,
			"merged":           true,
			"merged_at":        "2026-09-10T09:54:34Z",
			"closed_at":        "2026-09-10T09:54:34Z",
			"created_at":       "2026-09-10T09:00:00Z",
			"updated_at":       "2026-09-10T09:54:34Z",
			"merge_commit_sha": "ae18acf237df4b9862d52f82aa7d866052613507",
			"head":             map[string]any{"ref": "fix/ship-it"},
			"user":             map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    424242,
			"name":  repoName,
			"owner": map[string]any{"login": repoOwner},
		},
		"installation": map[string]any{"id": installationID},
	}
}

func postSignedGitHubWebhook(t *testing.T, secret string, body map[string]any, deliveryGUID string) *testutil.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(raw))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sig)
	if deliveryGUID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryGUID)
	}
	return testutil.Call(t, testHandler.HandleGitHubWebhook, req).Want(http.StatusAccepted)
}
