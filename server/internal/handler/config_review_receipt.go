package handler

import (
	"context"
	"errors"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6619 (CHE-6594 S1a): receipt parser over governed_vote_record.

var (
	errConfigReceiptNoRecord  = errors.New("no governed_vote_record for this comment")
	errConfigReceiptEmptyBody = errors.New("the vote record holds no comment body")
)

type configReceiptVote struct{ Verdict, Label, After, Before string }

type configReceiptRoute struct{ Mode, Label, After string }

// configReviewReceipt is what one governed_vote_record says.
type configReviewReceipt struct {
	CommentID string
	Votes     []configReceiptVote
	Routes    []configReceiptRoute
	Risks     []string
}

func parseConfigReviewReceipt(voteLines, fullBody string) (configReviewReceipt, error) {
	return configReviewReceipt{}, nil
}

func loadConfigReviewReceipt(ctx context.Context, q db.DBTX, workspaceID, issueID, commentID string) (configReviewReceipt, error) {
	return configReviewReceipt{}, nil
}
