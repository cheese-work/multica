package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CommentProtection reports whether agent runs are barred from editing another
// agent's comment. The server wires the real check; a nil one reads as false.
type CommentProtection func() bool

// VerifiedAgent is an agent identity with its lab from LabOf ("" when unknown).
type VerifiedAgent struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Lab       string `json:"lab"`
}

// VerifiedSigner is an agent whose trusted PASS stands on the exact head.
type VerifiedSigner struct {
	VerifiedAgent
	CommentID string    `json:"comment_id"`
	VerdictAt time.Time `json:"verdict_at"`
}

// VerifiedSignersResult holds facts only. Any merge rule lives in the consumer.
type VerifiedSignersResult struct {
	CommentProtection bool             `json:"comment_protection"`
	PRAuthor          *VerifiedAgent   `json:"pr_author"`
	Signers           []VerifiedSigner `json:"signers"`
}

// VerifiedSigners reads the distinct agents whose trusted PASS stands on c's
// exact head, the PR author, and whether comment protection is on (CHE-10267).
// The author comes from the workspace binding or the task that opened the PR,
// as the merge facts read it. An author that does not exist (no rows) is nil;
// any other lookup error is returned, with no signer facts. It reads one
// transaction and calls no network.
func VerifiedSigners(ctx context.Context, tx pgx.Tx, q *db.Queries, c MergeCandidate, protection CommentProtection) (VerifiedSignersResult, error) {
	var authorID pgtype.UUID
	if q != nil {
		pull, err := q.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{WorkspaceID: c.WorkspaceID, RepoOwner: c.Owner, RepoName: c.Repo, PrNumber: c.Number})
		switch {
		case err == nil:
			author, aerr := GitHubPullRequestAuthor(ctx, q, pull)
			switch {
			case aerr == nil:
				authorID = author.ID
			case !errors.Is(aerr, pgx.ErrNoRows):
				// A lookup fault is not an unknown author: counting signers without
				// the author could count the author as a signer.
				return VerifiedSignersResult{}, aerr
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return VerifiedSignersResult{}, err
		}
	}
	out, err := verifiedSigners(ctx, tx, c.WorkspaceID, c.IssueID, c.HeadSHA, authorID)
	if err != nil {
		return VerifiedSignersResult{}, err
	}
	out.CommentProtection = protection != nil && protection()
	return out, nil
}

// A verdict takes effect when it was written or last edited, whichever is
// later (comment_content_edit), so editing an old comment cannot reorder it
// behind a newer one. An edited verdict is never a PASS, but it still ends the
// signer's earlier PASS: an edit can only withdraw, never grant. That holds when
// the edit removes the verdict line or the head too: a comment edited after the
// PASS was written, no longer a verdict on this head, also ends that PASS.
//
// The trust rests on where a comment came from: author_id is stamped from the
// task token, and source_task_id must name a task of that same agent. A member's
// comment, an agent comment with no task, or one whose task belongs to another
// agent is not an agent run's verdict.
func verifiedSigners(ctx context.Context, tx pgx.Tx, workspaceID, issueID pgtype.UUID, head string, authorID pgtype.UUID) (VerifiedSignersResult, error) {
	out := VerifiedSignersResult{Signers: []VerifiedSigner{}}
	if authorID.Valid {
		if a, found, err := agentIdentity(ctx, tx, workspaceID, authorID); err != nil {
			return out, err
		} else if found {
			out.PRAuthor = &a
		}
	}
	rows, err := tx.Query(ctx, `SELECT c.id, c.author_id, c.content, c.created_at, COALESCE(e.edited_at, c.created_at) AS effective_at,
			e.comment_id IS NOT NULL AS edited,
			c.source_task_id IS NOT NULL AND EXISTS (SELECT 1 FROM agent_task_queue t WHERE t.id = c.source_task_id AND t.agent_id = c.author_id) AS from_own_task
		FROM comment c LEFT JOIN comment_content_edit e ON e.comment_id = c.id
		WHERE c.workspace_id=$1 AND c.issue_id=$2 AND c.author_type='agent' AND c.type='comment' AND c.deleted_at IS NULL AND (position($3 in c.content) > 0 OR e.comment_id IS NOT NULL)
		ORDER BY effective_at, c.id`, workspaceID, issueID, head)
	if err != nil {
		return out, err
	}
	type latest struct {
		pass      bool
		commentID pgtype.UUID
		at        time.Time
	}
	bySigner := map[pgtype.UUID]latest{}
	for rows.Next() {
		var id, signer pgtype.UUID
		var content string
		var created, effective time.Time
		var edited, ownTask bool
		if err := rows.Scan(&id, &signer, &content, &created, &effective, &edited, &ownTask); err != nil {
			rows.Close()
			return out, err
		}
		if word, h, ok := parseVerdict(content); ok && h == head {
			bySigner[signer] = latest{pass: word == "PASS" && ownTask && !edited, commentID: id, at: created}
		} else if prev, seen := bySigner[signer]; edited && seen && prev.pass && created.After(prev.at) {
			// The edit store keeps no prior text, so an edited comment that no
			// longer reads as a verdict on this head may have been a FAIL when it
			// was posted. Written after the PASS, it withdraws it.
			bySigner[signer] = latest{pass: false, commentID: id, at: created}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for signer, v := range bySigner {
		if !v.pass || signer == authorID {
			continue
		}
		a, found, err := agentIdentity(ctx, tx, workspaceID, signer)
		if err != nil {
			return out, err
		}
		if !found {
			a = VerifiedAgent{AgentID: util.UUIDToString(signer)}
		}
		out.Signers = append(out.Signers, VerifiedSigner{VerifiedAgent: a, CommentID: util.UUIDToString(v.commentID), VerdictAt: v.at})
	}
	sort.Slice(out.Signers, func(i, j int) bool { return out.Signers[i].AgentID < out.Signers[j].AgentID })
	return out, nil
}

// agentIdentity names an agent and its lab. An archived agent keeps its name
// and has no lab, as before: it is neither a signer nor an author the merge
// evaluator accepts. found is false for an agent this workspace does not have.
func agentIdentity(ctx context.Context, tx pgx.Tx, workspaceID, agentID pgtype.UUID) (VerifiedAgent, bool, error) {
	var name, model, provider string
	var archived bool
	err := tx.QueryRow(ctx, `SELECT a.name, COALESCE(a.model,''), COALESCE(r.provider,''), a.archived_at IS NOT NULL FROM agent a LEFT JOIN agent_runtime r ON r.id=a.runtime_id
		WHERE a.id=$1 AND a.workspace_id=$2`, agentID, workspaceID).Scan(&name, &model, &provider, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifiedAgent{}, false, nil
	}
	if err != nil {
		return VerifiedAgent{}, false, err
	}
	a := VerifiedAgent{AgentID: util.UUIDToString(agentID), AgentName: name}
	if !archived {
		a.Lab = LabOf(model, provider)
	}
	return a, true, nil
}
