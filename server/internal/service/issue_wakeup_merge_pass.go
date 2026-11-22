package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
)

// verdictLineRE is the contract of a signed verdict: the first line of the
// comment reads: **Verdict: WORD** ... head, then the head SHA in backticks.
// Only that line is read,
// so a quoted verdict further down never counts.
var verdictLineRE = regexp.MustCompile("^\\*\\*Verdict:\\s*([A-Za-z][A-Za-z ]*?)\\*\\*.*?\\bhead\\s+`([^`\\s]+)`")

// parseVerdict returns the verdict word and head of a first-hand verdict
// comment. A line carrying ", signed by" is a relay of someone else's verdict;
// the relaying agent did not sign it, so it is not one.
func parseVerdict(content string) (word, head string, ok bool) {
	first, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	if strings.Contains(strings.ToLower(first), "signed by") {
		return "", "", false
	}
	m := verdictLineRE.FindStringSubmatch(first)
	if m == nil {
		return "", "", false
	}
	return strings.ToUpper(strings.TrimSpace(m[1])), m[2], true
}

// LabOf names the lab behind an agent from its model, else its runtime
// provider. An agent it cannot place has no lab (""), and then it is neither a
// signer nor an author the evaluator accepts. The merge gate and reviewer
// routing share it. ponytail: closed set, add a lab here when a new provider
// joins.
func LabOf(model, provider string) string {
	if price, ok := obsmetrics.PriceForModelAlias(model); ok {
		return price.Provider
	}
	switch strings.ToLower(provider) {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	case "gemini", "agy":
		return "google"
	}
	return ""
}

func agentLab(ctx context.Context, tx pgx.Tx, workspaceID, agentID pgtype.UUID) (string, error) {
	var model, provider string
	err := tx.QueryRow(ctx, `SELECT COALESCE(a.model,''), COALESCE(r.provider,'') FROM agent a LEFT JOIN agent_runtime r ON r.id=a.runtime_id
		WHERE a.id=$1 AND a.workspace_id=$2 AND a.archived_at IS NULL`, agentID, workspaceID).Scan(&model, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return LabOf(model, provider), nil
}

// A verdict takes effect when it was written or last edited, whichever is
// later (comment_content_edit), so editing an old comment cannot reorder it
// behind a newer one or backdate the veto window.
//
// storedCrossLabPass derives the two PASS facts from Multica: the newest signed
// PASS from an agent of another lab than the PR author, on the exact head, in a
// comment on the wakeup's issue. The signer is the comment's author, so the
// text cannot name someone else. A signer's newest verdict on the head decides
// (a later non-PASS withdraws it). An unknown author or no qualifying signer
// gives no PASS. It reads one transaction and calls no network.
func storedCrossLabPass(ctx context.Context, tx pgx.Tx, c MergeCandidate, authorID pgtype.UUID) (*bool, *time.Time, error) {
	if !authorID.Valid {
		return nil, nil, nil
	}
	authorLab, err := agentLab(ctx, tx, c.WorkspaceID, authorID)
	if err != nil || authorLab == "" {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT c.author_id, c.content, COALESCE(e.edited_at, c.created_at) AS effective_at FROM comment c
		LEFT JOIN comment_content_edit e ON e.comment_id = c.id
		WHERE c.workspace_id=$1 AND c.issue_id=$2 AND c.author_type='agent' AND c.type='comment' AND c.deleted_at IS NULL AND position($3 in c.content) > 0
		ORDER BY effective_at, c.id`, c.WorkspaceID, c.IssueID, c.HeadSHA)
	if err != nil {
		return nil, nil, err
	}
	type latest struct {
		pass bool
		at   time.Time
	}
	bySigner := map[pgtype.UUID]latest{}
	for rows.Next() {
		var signer pgtype.UUID
		var content string
		var at time.Time
		if err := rows.Scan(&signer, &content, &at); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if word, head, ok := parseVerdict(content); ok && head == c.HeadSHA {
			bySigner[signer] = latest{pass: word == "PASS", at: at}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var newest *time.Time
	for signer, v := range bySigner {
		if !v.pass || signer == authorID {
			continue
		}
		lab, err := agentLab(ctx, tx, c.WorkspaceID, signer)
		if err != nil {
			return nil, nil, err
		}
		if lab == "" || lab == authorLab {
			continue
		}
		if newest == nil || v.at.After(*newest) {
			at := v.at
			newest = &at
		}
	}
	pass := newest != nil
	return &pass, newest, nil
}
