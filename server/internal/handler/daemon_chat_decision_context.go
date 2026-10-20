package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// chatDecisionContext tells a DM run which open decision request the human's
// reply may be answering. The ping is written by the server, so no resumed
// agent session contains it, and a bare reply ("A") has no subject without it.
func chatDecisionContext(ctx context.Context, queries *db.Queries, workspaceID, sessionID pgtype.UUID, firstReply pgtype.Timestamptz) (string, error) {
	requests, err := queries.ListOpenDecisionRequestsBeforeMessage(ctx, db.ListOpenDecisionRequestsBeforeMessageParams{WorkspaceID: workspaceID, SessionID: sessionID, Before: firstReply})
	if err != nil || len(requests) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Open decision request(s) in this DM, asked before the message below. A short or subject-less reply answers the earliest one:\n")
	for _, r := range requests {
		issueID, askID := uuidToString(r.IssueID), uuidToString(r.IssueCommentID)
		fmt.Fprintf(&b, "- Request %s on %s (issue %s), ask comment %s: %q\n  Record the answer: `multica issue comment add %s --decision-record --message <id> --parent %s`\n", uuidToString(r.ID), r.IssueKey, issueID, askID, r.Ask, issueID, askID)
	}
	b.WriteString("<id> is the reply's DM message id from `multica chat history --output json`.\n")
	return b.String(), nil
}
