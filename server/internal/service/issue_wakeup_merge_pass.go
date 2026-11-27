package service

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/reviewindependence"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
// signer nor an author the evaluator accepts. It is the fallback of ReviewValues
// while the review_independence setting is absent. ponytail: closed set, add a lab here when a new provider
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

// agentLab is the value of an agent through the shared resolver (the lab, while
// the review_independence setting is absent).
func agentLab(ctx context.Context, tx pgx.Tx, rv ReviewValues, workspaceID, agentID pgtype.UUID) (string, error) {
	var model, provider string
	err := tx.QueryRow(ctx, `SELECT COALESCE(a.model,''), COALESCE(r.provider,'') FROM agent a LEFT JOIN agent_runtime r ON r.id=a.runtime_id
		WHERE a.id=$1 AND a.workspace_id=$2 AND a.archived_at IS NULL`, agentID, workspaceID).Scan(&model, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return rv.AgentValue(provider, model), nil
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
	rv, err := ReviewValuesForWorkspace(ctx, tx, c.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	authorLab, err := agentLab(ctx, tx, rv, c.WorkspaceID, authorID)
	if err != nil || authorLab == "" {
		return nil, nil, err
	}
	authors, err := gateAuthorSet(ctx, tx, rv, c, authorID, authorLab)
	if err != nil {
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
		if !v.pass || signer == authorID || authors.AgentIDs[util.UUIDToString(signer)] {
			continue
		}
		lab, err := agentLab(ctx, tx, rv, c.WorkspaceID, signer)
		if err != nil {
			return nil, nil, err
		}
		if lab == "" || slices.Contains(authors.Values, lab) || slices.Contains(authors.Values, LabUnknown) {
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

// gateAuthorSet is the author set the merge gate judges a signer against. With
// the review_independence setting absent it is the PR's author alone, as before.
// With it present it is the same set routing uses (IssueAuthorSet): the values on
// the stored commits of every PR linked to the issue, else the values of every run at a work step, and in
// both cases the PR's author agent and its value (CHE-9319). Commits come from
// the stored PR snapshots, never from the network.
func gateAuthorSet(ctx context.Context, tx pgx.Tx, rv ReviewValues, c MergeCandidate, authorID pgtype.UUID, authorLab string) (AuthorSet, error) {
	set := AuthorSet{Values: []string{authorLab}, Evidence: map[string][]string{}, AgentIDs: map[string]bool{util.UUIDToString(authorID): true}}
	if !rv.Configured() {
		return set, nil
	}
	commits, err := linkedPRStoredCommits(ctx, tx, c)
	if err != nil {
		return AuthorSet{}, err
	}
	got, err := rv.IssueAuthorSet(ctx, db.New(tx), c.IssueID, commits)
	if err != nil {
		return AuthorSet{}, err
	}
	for _, v := range got.Values {
		if !slices.Contains(set.Values, v) {
			set.Values = append(set.Values, v)
		}
	}
	for id := range got.AgentIDs {
		set.AgentIDs[id] = true
	}
	return set, nil
}

// linkedPRStoredCommits is the stored commits of every PR linked to the issue,
// each read at its current head (the candidate's at the candidate head). It is nil
// when any linked PR has none stored for its head (missing, stale or unreadable),
// so the author set keeps the run-based rule, as routing does when it cannot read
// a commit list.
func linkedPRStoredCommits(ctx context.Context, tx pgx.Tx, c MergeCandidate) ([]reviewindependence.PRCommit, error) {
	rows, err := tx.Query(ctx, `SELECT p.repo_owner, p.repo_name, p.pr_number, p.head_sha, COALESCE(p.commit_identities, 'null'::jsonb)
		FROM github_pull_request p JOIN issue_pull_request ipr ON ipr.pull_request_id = p.id
		WHERE ipr.issue_id=$1 AND p.workspace_id=$2 ORDER BY p.id`, c.IssueID, c.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []reviewindependence.PRCommit
	seenCandidate := false
	for rows.Next() {
		var owner, repo, head string
		var number int32
		var raw []byte
		if err := rows.Scan(&owner, &repo, &number, &head, &raw); err != nil {
			return nil, err
		}
		if owner == c.Owner && repo == c.Repo && int64(number) == int64(c.Number) {
			head, seenCandidate = c.HeadSHA, true
		}
		commits, ok := reviewindependence.CommitsAt(raw, head)
		if !ok {
			return nil, nil
		}
		all = append(all, commits...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !seenCandidate {
		return nil, nil
	}
	return all, nil
}
