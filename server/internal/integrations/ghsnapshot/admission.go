package ghsnapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type ReviewRevision struct {
	Event, Action          string
	ObjectID               int64
	NodeID, BodyHash       string
	AuthorID, SenderID     int64
	AuthorType, SenderType string
	ReviewState            string
}

type ReviewAdmission struct {
	State, Reason string
}

func ReviewEventKind(event, action string) (string, string) {
	var objectKind, initial, withdrawal string
	switch event {
	case "pull_request_review":
		objectKind, initial, withdrawal = "review", "submitted", "dismissed"
	case "pull_request_review_comment":
		objectKind, initial, withdrawal = "review_comment", "created", "deleted"
	case "issue_comment":
		objectKind, initial, withdrawal = "issue_comment", "created", "deleted"
	default:
		return "", ""
	}
	switch action {
	case initial:
		return objectKind, "initial"
	case "edited":
		return objectKind, "edit"
	case withdrawal:
		return objectKind, "withdrawal"
	default:
		return objectKind, ""
	}
}

func (manager *Manager) CheckReviewRevision(ctx context.Context, installationID int64, revision ReviewRevision, trustedID int64) ReviewAdmission {
	if manager == nil {
		return (*Client)(nil).CheckReviewRevision(ctx, installationID, revision, trustedID)
	}
	return manager.client.CheckReviewRevision(ctx, installationID, revision, trustedID)
}

func (client *Client) CheckReviewRevision(ctx context.Context, installationID int64, revision ReviewRevision, trustedID int64) ReviewAdmission {
	objectKind, revisionKind := ReviewEventKind(revision.Event, revision.Action)
	if revisionKind == "" {
		return ReviewAdmission{"denied", "unsupported_event_action"}
	}
	if revisionKind == "withdrawal" {
		return ReviewAdmission{"denied", revision.Action}
	}
	if trustedID <= 0 || revision.AuthorID != trustedID || revision.AuthorType != "User" {
		return ReviewAdmission{"denied", "untrusted_author"}
	}
	if revision.SenderType != "User" || revision.SenderID != trustedID {
		return ReviewAdmission{"quarantined", "foreign_editor"}
	}
	if objectKind == "review" && revision.ReviewState != "approved" && revision.ReviewState != "commented" && revision.ReviewState != "changes_requested" {
		return ReviewAdmission{"denied", "unsupported_review_state"}
	}
	if !client.Enabled() || installationID <= 0 || revision.ObjectID <= 0 || revision.NodeID == "" {
		return ReviewAdmission{"deferred", "provenance_lookup_failed"}
	}
	node, err := client.reviewProvenance(ctx, installationID, revision.NodeID)
	databaseID, databaseErr := node.DatabaseID.Int64()
	if err != nil || databaseErr != nil || node.ID != revision.NodeID || databaseID != revision.ObjectID || node.Type != map[string]string{
		"review": "PullRequestReview", "review_comment": "PullRequestReviewComment", "issue_comment": "IssueComment",
	}[objectKind] || node.Body == nil || len(node.Editor) == 0 || len(node.LastEditedAt) == 0 || (objectKind == "review" && node.State == nil) {
		return ReviewAdmission{"deferred", "provenance_lookup_failed"}
	}
	var editor *struct {
		Type       string `json:"__typename"`
		DatabaseID int64  `json:"databaseId"`
	}
	var lastEditedAt *time.Time
	if json.Unmarshal(node.Editor, &editor) != nil || json.Unmarshal(node.LastEditedAt, &lastEditedAt) != nil {
		return ReviewAdmission{"deferred", "provenance_lookup_failed"}
	}
	if (lastEditedAt != nil || editor != nil || revisionKind == "edit") && (editor == nil || editor.Type != "User" || editor.DatabaseID <= 0) {
		return ReviewAdmission{"quarantined", "unknown_edit_provenance"}
	}
	if editor != nil && editor.DatabaseID != trustedID {
		return ReviewAdmission{"quarantined", "foreign_editor"}
	}
	if revisionKind == "initial" && (editor != nil || lastEditedAt != nil) {
		return ReviewAdmission{"quarantined", "initial_revision_edited"}
	}
	digest := sha256.Sum256([]byte(*node.Body))
	if hex.EncodeToString(digest[:]) != revision.BodyHash {
		return ReviewAdmission{"quarantined", "body_changed"}
	}
	if objectKind == "review" && strings.ToLower(*node.State) != revision.ReviewState {
		return ReviewAdmission{"quarantined", "review_state_changed"}
	}
	return ReviewAdmission{"admitted", "verified_revision"}
}

type reviewProvenance struct {
	ID           string          `json:"id"`
	DatabaseID   json.Number     `json:"databaseId"`
	Type         string          `json:"__typename"`
	Body         *string         `json:"body"`
	Editor       json.RawMessage `json:"editor"`
	LastEditedAt json.RawMessage `json:"lastEditedAt"`
	State        *string         `json:"state"`
}

func (client *Client) reviewProvenance(ctx context.Context, installationID int64, nodeID string) (reviewProvenance, error) {
	const query = `query($id: ID!) { node(id: $id) { id __typename
... on IssueComment { databaseId: fullDatabaseId body lastEditedAt editor { __typename ... on User { databaseId } } }
... on PullRequestReviewComment { databaseId: fullDatabaseId body lastEditedAt editor { __typename ... on User { databaseId } } }
... on PullRequestReview { databaseId: fullDatabaseId body state lastEditedAt editor { __typename ... on User { databaseId } } }
} }`
	var node reviewProvenance
	token, err := client.installationToken(ctx, installationID)
	if err != nil {
		return node, err
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": map[string]string{"id": nodeID}})
	if err != nil {
		return node, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(client.apiBase, "/")+"/graphql", bytes.NewReader(body))
	if err != nil {
		return node, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return node, errors.New("github: provenance lookup failed")
	}
	defer response.Body.Close()
	const limit = 1 << 20
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(responseBody) > limit || response.StatusCode != http.StatusOK {
		return node, errors.New("github: provenance response unavailable")
	}
	var result struct {
		Data struct {
			Node *reviewProvenance `json:"node"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(responseBody, &result) != nil || len(result.Errors) != 0 || result.Data.Node == nil {
		return node, errors.New("github: invalid provenance response")
	}
	return *result.Data.Node, nil
}
