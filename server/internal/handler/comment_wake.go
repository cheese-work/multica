package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// comment_wake (CHE-5907) decides what an unmentioned member comment starts.
// "conversation" (default) keeps the implicit routing: last agent in the
// thread, thread owners, assignee or squad leader, blocked-assignee add.
// "mentioned_only" starts runs only for the agents and squads the comment names.
// Scope: issue metadata overrides the workspace setting. Projects have no
// settings store yet, so there is no project scope.
const (
	commentWakeSetting       = "comment_wake"
	commentWakeConversation  = "conversation"
	commentWakeMentionedOnly = "mentioned_only"
)

func validCommentWake(mode string) bool {
	return mode == commentWakeConversation || mode == commentWakeMentionedOnly
}

// validateCommentWakeSetting refuses a comment_wake value outside the two modes.
// A missing key or null passes.
func validateCommentWakeSetting(raw any) error {
	settings, _ := raw.(map[string]any)
	value, ok := settings[commentWakeSetting]
	if !ok || value == nil {
		return nil
	}
	if mode, _ := value.(string); !validCommentWake(mode) {
		return fmt.Errorf("%s must be %s or %s", commentWakeSetting, commentWakeConversation, commentWakeMentionedOnly)
	}
	return nil
}

// commentWakeMentionedOnlyFor reports whether unmentioned member comments on
// the issue start no run. Unreadable or invalid values mean "conversation".
func (h *Handler) commentWakeMentionedOnlyFor(ctx context.Context, issue db.Issue) bool {
	if mode := commentWakeFrom(issue.Metadata); mode != "" {
		return mode == commentWakeMentionedOnly
	}
	workspace, err := h.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		slog.WarnContext(ctx, "comment_wake read failed; keeping conversation", "workspace_id", uuidToString(issue.WorkspaceID), "error", err)
		return false
	}
	return commentWakeFrom(workspace.Settings) == commentWakeMentionedOnly
}

// commentWakeFrom reads a valid comment_wake mode from a JSON object, or "".
func commentWakeFrom(blob []byte) string {
	var fields struct {
		Mode string `json:"comment_wake"`
	}
	if json.Unmarshal(blob, &fields) != nil || !validCommentWake(fields.Mode) {
		return ""
	}
	return fields.Mode
}
