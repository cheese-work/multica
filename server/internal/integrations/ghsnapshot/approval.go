package ghsnapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ApprovalSnapshot is one API read of a review or comment (CHE-2126 plan 5.3,
// R3). Hash covers every field except ReadAt and never the body text, so the
// same content read twice hashes alike.
type ApprovalSnapshot struct {
	ObjectKind   string
	ObjectID     int64
	AuthorID     int64
	BodyHash     string
	ReviewState  string
	EditorType   string
	EditorID     int64
	LastEditedAt *time.Time
	ReadAt       time.Time
	Hash         string
}

// ContentHash is the digest an approver signs off on.
func (snapshot ApprovalSnapshot) ContentHash() string {
	edited := ""
	if snapshot.LastEditedAt != nil {
		edited = snapshot.LastEditedAt.UTC().Format(time.RFC3339Nano)
	}
	canonical, _ := json.Marshal([]any{
		snapshot.ObjectKind, snapshot.ObjectID, snapshot.AuthorID, snapshot.BodyHash, snapshot.ReviewState,
		snapshot.EditorType, snapshot.EditorID, edited,
	})
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

type userRef struct {
	Type       string `json:"__typename"`
	DatabaseID int64  `json:"databaseId"`
}

// ReadApprovalSnapshot resolves the object's GraphQL node through its REST
// read, then reads author, editor, last edit time and body through GraphQL. A
// missing or malformed field is an error: an incomplete read is never approvable.
func (client *Client) ReadApprovalSnapshot(ctx context.Context, installationID int64, owner, repo string, number int32, objectKind string, objectID int64) (ApprovalSnapshot, error) {
	if !client.Enabled() {
		return ApprovalSnapshot{}, errors.New("github: snapshot API is not configured")
	}
	base := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	var path string
	switch objectKind {
	case "review":
		path = fmt.Sprintf("%s/pulls/%d/reviews/%d", base, number, objectID)
	case "review_comment":
		path = fmt.Sprintf("%s/pulls/comments/%d", base, objectID)
	case "issue_comment":
		path = fmt.Sprintf("%s/issues/comments/%d", base, objectID)
	default:
		return ApprovalSnapshot{}, errors.New("github: unsupported snapshot object kind")
	}
	data, err := client.restGet(ctx, installationID, path)
	if err != nil {
		return ApprovalSnapshot{}, err
	}
	var rest struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
	}
	if json.Unmarshal(data, &rest) != nil || rest.ID != objectID || rest.NodeID == "" {
		return ApprovalSnapshot{}, errors.New("github: malformed snapshot object")
	}
	node, err := client.reviewProvenance(ctx, installationID, rest.NodeID)
	if err != nil {
		return ApprovalSnapshot{}, err
	}
	databaseID, databaseErr := node.DatabaseID.Int64()
	wantType := map[string]string{"review": "PullRequestReview", "review_comment": "PullRequestReviewComment", "issue_comment": "IssueComment"}[objectKind]
	if databaseErr != nil || databaseID != objectID || node.Type != wantType || node.Body == nil ||
		len(node.Author) == 0 || len(node.Editor) == 0 || len(node.LastEditedAt) == 0 || (objectKind == "review" && node.State == nil) {
		return ApprovalSnapshot{}, errors.New("github: incomplete snapshot read")
	}
	var author, editor *userRef
	var lastEditedAt *time.Time
	if json.Unmarshal(node.Author, &author) != nil || json.Unmarshal(node.Editor, &editor) != nil || json.Unmarshal(node.LastEditedAt, &lastEditedAt) != nil {
		return ApprovalSnapshot{}, errors.New("github: malformed snapshot read")
	}
	if author == nil || author.Type != "User" || author.DatabaseID <= 0 {
		return ApprovalSnapshot{}, errors.New("github: snapshot author is not a user")
	}
	digest := sha256.Sum256([]byte(*node.Body))
	snapshot := ApprovalSnapshot{
		ObjectKind: objectKind, ObjectID: objectID, AuthorID: author.DatabaseID, BodyHash: hex.EncodeToString(digest[:]),
		LastEditedAt: lastEditedAt, ReadAt: client.now(),
	}
	if node.State != nil {
		snapshot.ReviewState = strings.ToLower(*node.State)
	}
	if editor != nil {
		snapshot.EditorType, snapshot.EditorID = editor.Type, editor.DatabaseID
	}
	snapshot.Hash = snapshot.ContentHash()
	return snapshot, nil
}

func (manager *Manager) ReadApprovalSnapshot(ctx context.Context, installationID int64, owner, repo string, number int32, objectKind string, objectID int64) (ApprovalSnapshot, error) {
	if manager == nil || !manager.client.Enabled() {
		return ApprovalSnapshot{}, errors.New("github: snapshot API is not configured")
	}
	return manager.client.ReadApprovalSnapshot(ctx, installationID, owner, repo, number, objectKind, objectID)
}
