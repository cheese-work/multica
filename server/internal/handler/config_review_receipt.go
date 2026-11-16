package handler

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6619 (CHE-6594 S1a): receipt parser over governed_vote_record.
//
// A config-review receipt is one reviewer comment that carries, in one body:
//
//	Vote: PASS <field>:<target> <after_sha256> from <before_sha256>
//	Route: apply|cheese <field>:<target> <after_sha256>
//	Risk: <one sentence>
//
// Migration 809 snapshots such a comment into governed_vote_record when it is
// inserted: the Vote line in vote_lines, the whole body in full_body. This
// parser reads Vote lines from vote_lines and Route and Risk lines from
// full_body of that same row. It never reads the live comment, so editing or
// deleting the comment afterwards changes nothing, and a Route line in
// another comment is never part of this record.
//
// The parser only extracts lines. Whether a receipt is acceptable (exactly one
// of each line, matching digests, reviewer and lab) is the shared receipt
// core's job (CHE-6594 S1b).

var (
	errConfigReceiptNoRecord  = errors.New("no governed_vote_record for this comment")
	errConfigReceiptEmptyBody = errors.New("the vote record holds no comment body")
)

// configReceiptVote is one `Vote:` line. Before is empty when the line has no
// `from <before_sha256>` suffix. Malformed is true when the line does not have
// the receipt shape; its other fields are then empty and only Raw is set.
type configReceiptVote struct {
	Verdict, Label, After, Before string
	Raw                           string
	Malformed                     bool
}

// configReceiptRoute is one `Route:` line. Malformed as for configReceiptVote.
type configReceiptRoute struct {
	Mode, Label, After string
	Raw                string
	Malformed          bool
}

// configReviewReceipt holds every recognized line of each kind, in order. A
// malformed or empty line is kept (Votes and Routes flag it Malformed; an empty
// `Risk:` is the empty string), never dropped, so a repeated line stays visible
// to the validator, which must see exactly one of each (S1b).
type configReviewReceipt struct {
	CommentID string
	Votes     []configReceiptVote
	Routes    []configReceiptRoute
	Risks     []string
}

// configReceiptLines splits text the way the migration 809 trigger does: on
// \r?\n, trimming spaces from each line.
func configReceiptLines(text string) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.Trim(l, " ")
	}
	return lines
}

// parseConfigReviewReceipt reads Vote lines from voteLines and Route and Risk
// lines from fullBody. A record with an empty body (written before migration
// 809) cannot hold a Route line and is not a receipt.
func parseConfigReviewReceipt(voteLines, fullBody string) (configReviewReceipt, error) {
	if strings.TrimSpace(fullBody) == "" {
		return configReviewReceipt{}, errConfigReceiptEmptyBody
	}
	var r configReviewReceipt
	for _, line := range configReceiptLines(voteLines) {
		rest, ok := strings.CutPrefix(line, "Vote:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		switch {
		case len(f) == 3:
			r.Votes = append(r.Votes, configReceiptVote{Verdict: f[0], Label: f[1], After: f[2], Raw: line})
		case len(f) == 5 && f[3] == "from":
			r.Votes = append(r.Votes, configReceiptVote{Verdict: f[0], Label: f[1], After: f[2], Before: f[4], Raw: line})
		default:
			r.Votes = append(r.Votes, configReceiptVote{Raw: line, Malformed: true})
		}
	}
	for _, line := range configReceiptLines(fullBody) {
		if rest, ok := strings.CutPrefix(line, "Route:"); ok {
			if f := strings.Fields(rest); len(f) == 3 {
				r.Routes = append(r.Routes, configReceiptRoute{Mode: f[0], Label: f[1], After: f[2], Raw: line})
			} else {
				r.Routes = append(r.Routes, configReceiptRoute{Raw: line, Malformed: true})
			}
		} else if rest, ok := strings.CutPrefix(line, "Risk:"); ok {
			r.Risks = append(r.Risks, strings.TrimSpace(rest))
		}
	}
	return r, nil
}

// loadConfigReviewReceipt parses the governed_vote_record row of one comment.
// It reads that table only, never the comment.
func loadConfigReviewReceipt(ctx context.Context, q db.DBTX, workspaceID, issueID, commentID string) (configReviewReceipt, error) {
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return configReviewReceipt{}, err
	}
	issueUUID, err := util.ParseUUID(issueID)
	if err != nil {
		return configReviewReceipt{}, err
	}
	commentUUID, err := util.ParseUUID(commentID)
	if err != nil {
		return configReviewReceipt{}, err
	}
	var voteLines, fullBody string
	err = q.QueryRow(ctx, `
		SELECT vote_lines, full_body FROM governed_vote_record
		WHERE workspace_id = $1 AND issue_id = $2 AND comment_id = $3`,
		wsUUID, issueUUID, commentUUID).Scan(&voteLines, &fullBody)
	if errors.Is(err, pgx.ErrNoRows) {
		return configReviewReceipt{}, errConfigReceiptNoRecord
	}
	if err != nil {
		return configReviewReceipt{}, err
	}
	r, err := parseConfigReviewReceipt(voteLines, fullBody)
	if err != nil {
		return configReviewReceipt{}, err
	}
	r.CommentID = uuidToString(commentUUID)
	return r, nil
}
