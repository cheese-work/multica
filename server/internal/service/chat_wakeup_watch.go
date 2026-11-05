package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	chatWatchEventComment       = "comment.created"
	maxChatWatchQuote           = 2000
	chatWatchTickBatch    int32 = 100
)

func validChatWatch(in ChatWakeupInput) bool {
	if (in.Event == "") == (in.UntilStatus == "") || (in.Event != "" && in.Event != chatWatchEventComment) {
		return false
	}
	if in.ActorType == "" && !in.ActorID.Valid {
		return true
	}
	return in.Event != "" && (in.ActorType == "member" || in.ActorType == "agent")
}

func watchAccessErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrChatWatchIssueAccess
	}
	return fmt.Errorf("check issue access: %w", err)
}

func onTimeout(wake bool) string {
	if wake {
		return "wake"
	}
	return "drop"
}

// expiryContent is the turn for a wakeup whose fire_at passed: a plain
// wakeup's instruction, a timed-out watch's notice, or "" to drop the watch.
func expiryContent(w db.ChatWakeup) string {
	switch {
	case !w.IssueID.Valid:
		return chatWakeupContent(w.Instruction)
	case w.OnTimeout == "wake":
		return chatWatchContent(w, "The watch timed out before any matching event; the issue is "+util.UUIDToString(w.IssueID)+".")
	}
	return ""
}

func chatWatchContent(w db.ChatWakeup, what string) string {
	return "[Issue watch — you set this earlier in this DM; no person wrote this message.]\nInstruction: " + w.Instruction + "\n" + what
}

// tickWatches fires every pending issue watch whose event or status has arrived.
// A watch that cannot be checked or fired stays pending for the next tick.
func (s *ChatWakeupService) tickWatches(ctx context.Context) error {
	q := s.Tasks.Queries
	watches, err := q.ListPendingChatIssueWatches(ctx, chatWatchTickBatch)
	if err != nil {
		return fmt.Errorf("list chat issue watches: %w", err)
	}
	var errs []error
	for _, w := range watches {
		what, err := s.watchHit(ctx, w)
		if err == nil && what != "" {
			err = s.fire(ctx, w, chatWatchContent(w, what))
		}
		if err != nil {
			slog.Warn("chat issue watch failed", "wakeup_id", util.UUIDToString(w.ID), "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// watchHit describes the matching event, or "" when none yet.
func (s *ChatWakeupService) watchHit(ctx context.Context, w db.ChatWakeup) (string, error) {
	q := s.Tasks.Queries
	issue := util.UUIDToString(w.IssueID)
	if w.UntilStatus != "" {
		issueRow, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: w.IssueID, WorkspaceID: w.WorkspaceID})
		if err != nil || issueRow.Status != w.UntilStatus {
			if errors.Is(err, pgx.ErrNoRows) {
				err = nil
			}
			return "", err
		}
		return fmt.Sprintf("Event: issue %s reached status %q.", issue, issueRow.Status), nil
	}
	c, err := q.FirstMatchingIssueComment(ctx, db.FirstMatchingIssueCommentParams{
		IssueID: w.IssueID, Since: w.CreatedAt, ActorType: w.FilterActorType, ActorID: w.FilterActorID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	body := c.Content
	if r := []rune(body); len(r) > maxChatWatchQuote {
		body = string(r[:maxChatWatchQuote]) + "…"
	}
	return fmt.Sprintf("Event: %s on issue %s. Comment %s by %s %s:\n> %s", chatWatchEventComment, issue, util.UUIDToString(c.ID), c.AuthorType, util.UUIDToString(c.AuthorID), body), nil
}
