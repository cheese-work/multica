package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// StoredMergeFacts is the production MergeFactsSource (CHE-5594). It reads only
// stored state, in one short read-only transaction: the PR snapshot row, the
// pause variables the refresher keeps, and the receipt facts recorded for the
// exact PR head in merge_receipt_fact. The database invalidates a receipt on
// every PR event that could revoke it, a Multica-only veto holds the PR, and
// the receipt expires on its own age. It calls no network and runs no PR code.
// A fact it cannot read stays absent, which the evaluator refuses.
type StoredMergeFacts struct {
	Tx      TxStarter
	Queries *db.Queries
}

var errMergeReceiptMissing = errors.New("no receipt facts are stored for this PR head and base")

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
	var checks pgtype.Text
	var head, snapshotHead string
	var snapshotAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `SELECT checks_rollup_state, head_sha, snapshot_head_sha, snapshot_fetched_at FROM github_pull_request
		WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND pr_number=$4`,
		c.WorkspaceID, c.Owner, c.Repo, c.Number).Scan(&checks, &head, &snapshotHead, &snapshotAt)
	if err != nil {
		return MergeFacts{}, fmt.Errorf("read PR snapshot: %w", err)
	}
	// Facts describe one head; a PR that moved since the candidate was read has none.
	if head != c.HeadSHA || snapshotHead != c.HeadSHA {
		return MergeFacts{}, errMergeReceiptMissing
	}
	green := checks.Valid && checks.String == "SUCCESS"
	f.ChecksGreen = &green
	// The author comes from the workspace binding or the task that opened the PR,
	// never from the branch name alone. An unknown author stays absent and refuses.
	if s.Queries != nil {
		if pull, err := s.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{WorkspaceID: c.WorkspaceID, RepoOwner: c.Owner, RepoName: c.Repo, PrNumber: c.Number}); err == nil {
			if author, err := GitHubPullRequestAuthor(ctx, s.Queries, pull); err == nil {
				same := author.ID == c.MergerAgentID
				f.MergerIsAuthor = &same
			}
		}
	}

	f.Paused, f.MainRedPaused, err = storedPauseFacts(ctx, tx, c.Owner, c.Repo)
	if err != nil {
		return MergeFacts{}, err
	}
	pausesObserved, err := storedPauseObservedAt(ctx, tx, c.Owner, c.Repo)
	if err != nil {
		return MergeFacts{}, err
	}

	// A veto recorded in Multica holds the PR whatever receipt is stored.
	var vetoed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM merge_veto WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND pr_number=$4)`,
		c.WorkspaceID, c.Owner, c.Repo, c.Number).Scan(&vetoed); err != nil {
		return MergeFacts{}, fmt.Errorf("read veto: %w", err)
	}
	if vetoed {
		return MergeFacts{}, errMergeVetoed
	}

	var paths, labels []string
	err = tx.QueryRow(ctx, `SELECT tier,classifier_trusted,coverage_complete,pr_bound,security_gate,cross_lab_pass,scope_classified,
			gate_vetoed,paths,labels,last_pass_at,evidence,observed_at
		FROM merge_receipt_fact WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND pr_number=$4 AND head_sha=$5 AND base_ref=$6`,
		c.WorkspaceID, c.Owner, c.Repo, c.Number, c.HeadSHA, c.BaseRef).Scan(&f.Tier, &f.ClassifierTrusted, &f.CoverageComplete, &f.PRBound,
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
	// Every input carries its own age and the oldest one bounds the facts: the
	// receipt (stamped when the producer recorded it), the PR snapshot and the
	// pause reads. A lost webhook or a revocation nobody reported leaves a
	// receipt in place, but it stops counting once the policy bound passes. The
	// producer re-records inside that bound, which is also what lets a veto
	// deadline that passes long after the PASS dispatch.
	if snapshotAt.Valid && snapshotAt.Time.Before(f.ObservedAt) {
		f.ObservedAt = snapshotAt.Time
	}
	if !snapshotAt.Valid {
		f.ObservedAt = time.Time{}
	}
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

// MergeReceiptInput is one receipt's facts for one PR head and base. A nil
// pointer is an absent fact, which the evaluator refuses.
type MergeReceiptInput struct {
	Owner, Repo, HeadSHA, BaseRef string
	Number                        int32
	Tier                          *int
	ClassifierTrusted             *bool
	CoverageComplete              *bool
	PRBound                       *bool
	SecurityGate                  string
	CrossLabPass                  *bool
	ScopeClassified               *bool
	GateVetoed                    *bool
	Paths, Labels                 []string
	LastPassAt                    *time.Time
	Evidence                      string
}

var (
	errMergeReceiptInvalid  = errors.New("receipt needs repo, PR number, head, base and evidence")
	errMergeReceiptNotFresh = errors.New("receipt does not match the PR's current head and base")
)

// IsMergeReceiptConflict reports that the receipt describes a head or base the
// stored PR snapshot no longer has.
func IsMergeReceiptConflict(err error) bool {
	return errors.Is(err, errMergeReceiptNotFresh) || errors.Is(err, errMergeVetoed)
}

// IsMergeReceiptInvalid reports a receipt missing a required field.
func IsMergeReceiptInvalid(err error) bool { return errors.Is(err, errMergeReceiptInvalid) }

// RecordMergeReceipt is the producer of merge_receipt_fact. The caller must
// already have checked that the credential may assert receipt facts; this stores
// them only when the PR is open and its stored snapshot has exactly this head
// and base, and stamps the observation time itself. Rows for any other head or
// base of the PR are dropped, so a stale receipt cannot come back.
func RecordMergeReceipt(ctx context.Context, txs TxStarter, workspaceID pgtype.UUID, in MergeReceiptInput, now time.Time) error {
	if in.Owner == "" || in.Repo == "" || in.Number <= 0 || in.HeadSHA == "" || in.BaseRef == "" || in.Evidence == "" {
		return errMergeReceiptInvalid
	}
	tx, err := txs.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state, head, snapshotHead, base string
	err = tx.QueryRow(ctx, `SELECT state, head_sha, snapshot_head_sha, COALESCE(snapshot_base_ref,'') FROM github_pull_request
		WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND pr_number=$4 FOR UPDATE`,
		workspaceID, in.Owner, in.Repo, in.Number).Scan(&state, &head, &snapshotHead, &base)
	if errors.Is(err, pgx.ErrNoRows) {
		return errMergeReceiptNotFresh
	}
	if err != nil {
		return err
	}
	if state != "open" || head != in.HeadSHA || snapshotHead != in.HeadSHA || base != in.BaseRef {
		return errMergeReceiptNotFresh
	}
	// Nothing recorded under a veto may outlive it.
	var vetoed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM merge_veto WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND pr_number=$4)`,
		workspaceID, in.Owner, in.Repo, in.Number).Scan(&vetoed); err != nil {
		return err
	}
	if vetoed {
		return errMergeVetoed
	}
	if _, err := tx.Exec(ctx, `DELETE FROM merge_receipt_fact WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND pr_number=$4`,
		workspaceID, in.Owner, in.Repo, in.Number); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO merge_receipt_fact(workspace_id,repo_owner,repo_name,pr_number,head_sha,base_ref,tier,classifier_trusted,
			coverage_complete,pr_bound,security_gate,cross_lab_pass,scope_classified,gate_vetoed,paths,labels,last_pass_at,evidence,observed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		workspaceID, in.Owner, in.Repo, in.Number, in.HeadSHA, in.BaseRef, in.Tier, in.ClassifierTrusted, in.CoverageComplete, in.PRBound,
		in.SecurityGate, in.CrossLabPass, in.ScopeClassified, in.GateVetoed, in.Paths, in.Labels, in.LastPassAt, in.Evidence, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
