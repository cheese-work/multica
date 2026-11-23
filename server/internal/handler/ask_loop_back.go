package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Loop back (T5c, CHE-6434): when a member answers in an ask thread, the thread
// it was opened from gets one system line, so the agents there see the answer
// without reading the ask thread. The line has no mention, starts no run and
// changes no resolution. It is written after the member's own write commits and
// a failure only logs at INFO: the member's comment or card answer never fails.

const (
	askLoopBackKey      = "ask_loop_back"
	askLoopBackOn       = "on"
	askLoopBackOff      = "off"
	askLoopBackQuoteMax = 280
)

// askLoopBackEnabled reads the setting. Only the stored string "on" turns it
// on; a missing, null or invalid value is off, so a comment never fails
// because of a setting.
func askLoopBackEnabled(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return false
	}
	mode, _ := settings[askLoopBackKey].(string)
	return mode == askLoopBackOn
}

// validateAskLoopBackSetting checks the ask_loop_back key of a workspace
// settings object. An absent key is valid; null is not.
func validateAskLoopBackSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	v, present := config[askLoopBackKey]
	if !present {
		return nil
	}
	if mode, _ := v.(string); mode != askLoopBackOn && mode != askLoopBackOff {
		return fmt.Errorf("%s must be off or on", askLoopBackKey)
	}
	return nil
}

// askLoopBackQuote is the member's text for the source thread: mention links
// reduced to their names (the line must mention nobody), cut to the first 280
// characters with an ellipsis when it was longer.
func askLoopBackQuote(content string) string {
	text := strings.TrimSpace(util.MentionRe.ReplaceAllString(content, "$1"))
	if utf8.RuneCountInString(text) <= askLoopBackQuoteMax {
		return text
	}
	return string([]rune(text)[:askLoopBackQuoteMax]) + "…"
}

// loopBackMemberComment echoes a member's comment in an ask thread into the
// source thread. Called once per created comment, never for an edit.
func (h *Handler) loopBackMemberComment(ctx context.Context, issue db.Issue, comment db.Comment) {
	if comment.AuthorType != "member" || !comment.ParentID.Valid || comment.Type != "comment" {
		return
	}
	root, err := h.Queries.GetThreadRoot(ctx, db.GetThreadRootParams{CommentID: comment.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil || !root.AskSourceRootID.Valid || root.ID == comment.ID {
		return
	}
	name := h.loopBackMemberName(ctx, comment.AuthorID)
	text := fmt.Sprintf("%s answered in thread %s:\n\n%s", name, uuidToString(root.ID), blockQuote(askLoopBackQuote(comment.Content)))
	h.writeAskLoopBack(ctx, issue, root.AskSourceRootID, comment.ID, text)
}

// loopBackDecisionAnswer echoes a decision answer whose ask comment sits in an
// ask thread. The answer's own quote comment is agent-authored, so a card
// answer never reaches loopBackMemberComment.
func (h *Handler) loopBackDecisionAnswer(ctx context.Context, decision db.DecisionRequest, answer db.DecisionAnswer, member pgtype.UUID) {
	root, err := h.Queries.GetThreadRoot(ctx, db.GetThreadRootParams{CommentID: decision.IssueCommentID, WorkspaceID: decision.WorkspaceID})
	if err != nil || !root.AskSourceRootID.Valid {
		return
	}
	issue, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: decision.WorkspaceID, ID: decision.IssueID})
	if err != nil {
		slog.Info("ask loop-back skipped: issue not found", "decision_request_id", uuidToString(decision.ID), "error", err)
		return
	}
	text := fmt.Sprintf("%s decided: %s (thread %s).", h.loopBackMemberName(ctx, member), askLoopBackAnswerLabel(decision, answer), uuidToString(root.ID))
	h.writeAskLoopBack(ctx, issue, root.AskSourceRootID, answer.IssueCommentID, text)
}

// askLoopBackAnswerLabel names what the member chose: the stored label, then
// the option at the chosen index, then the first line of the answer.
func askLoopBackAnswerLabel(decision db.DecisionRequest, answer db.DecisionAnswer) string {
	if answer.AnsweredLabel.Valid && answer.AnsweredLabel.String != "" {
		return answer.AnsweredLabel.String
	}
	if answer.ChosenOptionIndex.Valid {
		var options []struct {
			Label string `json:"label"`
		}
		if json.Unmarshal(decision.Options, &options) == nil && int(answer.ChosenOptionIndex.Int32) < len(options) && options[answer.ChosenOptionIndex.Int32].Label != "" {
			return options[answer.ChosenOptionIndex.Int32].Label
		}
	}
	first, _, _ := strings.Cut(strings.TrimSpace(util.MentionRe.ReplaceAllString(answer.ContentSnapshot, "$1")), "\n")
	if utf8.RuneCountInString(first) > 80 {
		first = string([]rune(first)[:80]) + "…"
	}
	return first
}

func (h *Handler) loopBackMemberName(ctx context.Context, userID pgtype.UUID) string {
	user, err := h.Queries.GetUser(ctx, userID)
	if err != nil || strings.TrimSpace(user.Name) == "" {
		return "A member"
	}
	return strings.Join(strings.Fields(strings.NewReplacer("[", "", "]", "").Replace(user.Name)), " ")
}

// writeAskLoopBack writes the system reply under the source root when the
// setting is on and the source root still exists. Any failure logs at INFO.
func (h *Handler) writeAskLoopBack(ctx context.Context, issue db.Issue, sourceRootID, triggerID pgtype.UUID, text string) {
	workspace, err := h.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		slog.Info("ask loop-back skipped: workspace not read", "issue_id", uuidToString(issue.ID), "error", err)
		return
	}
	if !askLoopBackEnabled(workspace.Settings) {
		return
	}
	source, err := h.Queries.GetComment(ctx, sourceRootID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (source.DeletedAt.Valid || source.IssueID != issue.ID) {
		slog.Info("ask loop-back skipped: source thread root is gone", "source_root_id", uuidToString(sourceRootID), "comment_id", uuidToString(triggerID))
		return
	}
	if err != nil {
		slog.Info("ask loop-back skipped: source root not read", "source_root_id", uuidToString(sourceRootID), "error", err)
		return
	}
	line, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		ID:          dbid.NewV7(),
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     text,
		Type:        "comment",
		ParentID:    sourceRootID,
	})
	if err != nil {
		slog.Info("ask loop-back not written", "source_root_id", uuidToString(sourceRootID), "comment_id", uuidToString(triggerID), "error", err)
		return
	}
	h.publishAskSourceStub(issue, line)
}
