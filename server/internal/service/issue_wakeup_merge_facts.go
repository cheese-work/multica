package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// StoredMergeFacts is the production MergeFactsSource (CHE-5594). It reads only
// stored state, in one short read-only transaction: the PR snapshot row, the
// pause variables the refresher keeps, and the receipt facts recorded for the
// exact PR head in merge_receipt_fact. It calls no network and runs no PR code.
// A fact it cannot read stays absent, which the evaluator refuses.
type StoredMergeFacts struct {
	Tx TxStarter
}

var errMergeReceiptMissing = errors.New("no receipt facts are stored for this PR head")

// agentBranchAuthor returns the agent name of an `agent/<name>/...` branch.
func agentBranchAuthor(branch string) (string, bool) {
	parts := strings.SplitN(branch, "/", 3)
	if len(parts) < 3 || parts[0] != "agent" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func (s *StoredMergeFacts) MergeFacts(ctx context.Context, c MergeCandidate) (MergeFacts, error) {
	if s == nil || s.Tx == nil {
		return MergeFacts{}, errors.New("stored merge facts have no database")
	}
	tx, err := s.Tx.Begin(ctx)
	if err != nil {
		return MergeFacts{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var f MergeFacts
	var branch, checks pgtype.Text
	var head, snapshotHead string
	err = tx.QueryRow(ctx, `SELECT branch, checks_rollup_state, head_sha, snapshot_head_sha FROM github_pull_request
		WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND pr_number=$4`,
		c.WorkspaceID, c.Owner, c.Repo, c.Number).Scan(&branch, &checks, &head, &snapshotHead)
	if err != nil {
		return MergeFacts{}, fmt.Errorf("read PR snapshot: %w", err)
	}
	// Facts describe one head; a PR that moved since the candidate was read has none.
	if head != c.HeadSHA || snapshotHead != c.HeadSHA {
		return MergeFacts{}, errMergeReceiptMissing
	}
	green := checks.Valid && checks.String == "SUCCESS"
	f.ChecksGreen = &green
	var merger string
	if err := tx.QueryRow(ctx, `SELECT name FROM agent WHERE id=$1`, c.MergerAgentID).Scan(&merger); err != nil {
		return MergeFacts{}, fmt.Errorf("read merger agent: %w", err)
	}
	if author, ok := agentBranchAuthor(branch.String); ok {
		same := strings.EqualFold(author, merger)
		f.MergerIsAuthor = &same
	}

	f.Paused, f.MainRedPaused, err = storedPauseFacts(ctx, tx, c.Owner, c.Repo)
	if err != nil {
		return MergeFacts{}, err
	}
	pausesObserved, err := storedPauseObservedAt(ctx, tx, c.Owner, c.Repo)
	if err != nil {
		return MergeFacts{}, err
	}

	var paths, labels []string
	err = tx.QueryRow(ctx, `SELECT tier,classifier_trusted,coverage_complete,pr_bound,security_gate,cross_lab_pass,scope_classified,
			gate_vetoed,paths,labels,last_pass_at,evidence,observed_at
		FROM merge_receipt_fact WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND pr_number=$4 AND head_sha=$5`,
		c.WorkspaceID, c.Owner, c.Repo, c.Number, c.HeadSHA).Scan(&f.Tier, &f.ClassifierTrusted, &f.CoverageComplete, &f.PRBound,
		&f.SecurityGate, &f.CrossLabPass, &f.ScopeClassified, &f.GateVetoed, &paths, &labels, &f.LastPassAt, &f.Evidence, &f.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MergeFacts{}, errMergeReceiptMissing
	}
	if err != nil {
		return MergeFacts{}, fmt.Errorf("read receipt facts: %w", err)
	}
	f.Paths = paths
	if labels != nil {
		f.Labels = &labels
	}
	// The facts are only as fresh as the oldest input.
	if pausesObserved.Before(f.ObservedAt) {
		f.ObservedAt = pausesObserved
	}
	return f, nil
}

// storedPauseFacts reports each pause variable as set, not set or unknown (nil).
func storedPauseFacts(ctx context.Context, tx pgx.Tx, owner, repo string) (paused, mainRed *bool, err error) {
	rows, err := tx.Query(ctx, `SELECT variable_name, is_set FROM merge_pause_state WHERE repo_owner=$1 AND repo_name=$2`, owner, repo)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var set *bool
		if err := rows.Scan(&name, &set); err != nil {
			return nil, nil, err
		}
		switch name {
		case "AGENT_MERGE_PAUSED":
			paused = set
		case "MAIN_RED_PAUSED":
			mainRed = set
		}
	}
	return paused, mainRed, rows.Err()
}

// storedPauseObservedAt is the oldest pause read; with no rows it is the zero
// time, which the evaluator refuses as undated.
func storedPauseObservedAt(ctx context.Context, tx pgx.Tx, owner, repo string) (time.Time, error) {
	var oldest pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT min(fetched_at) FROM merge_pause_state WHERE repo_owner=$1 AND repo_name=$2`, owner, repo).Scan(&oldest)
	return oldest.Time, err
}
