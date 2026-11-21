// Package backfill runs the issuecreatedvia classifier over old issues
// (CHE-6376 S2). The dry run reports what would change; apply writes only
// issue.created_via and a receipt; revert restores from that receipt.
package backfill

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/issuecreatedvia"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// lockKey is the session advisory lock that keeps two runs apart.
const lockKey int64 = 63760002

const (
	defaultBatchSize = 500
	maxListed        = 20
	maxSamples       = 5
	// Unknown is the report bucket of rule 7 (integration channels): the row
	// keeps NULL.
	Unknown = "unknown"
)

// integrationOrigins are the origin types of rule 7: a member's /issue command
// in an integration channel. Those rows keep NULL (filter value `unknown`).
var integrationOrigins = map[string]bool{
	"slack_chat": true, "telegram_chat": true, "lark_chat": true, "dingtalk_chat": true, "wecom_chat": true,
}

// Options narrows a run. A zero WorkspaceID scans every workspace.
type Options struct {
	WorkspaceID pgtype.UUID
	// IssueIDs, when set, limits the run to these issues. The binary does not
	// expose it; tests use it to leave other rows of a shared workspace alone.
	IssueIDs  []pgtype.UUID
	BatchSize int
}

func (o Options) batch() int {
	if o.BatchSize > 0 {
		return o.BatchSize
	}
	return defaultBatchSize
}

// Sample is one legacy `comment` row with the comment that triggered it.
type Sample struct {
	IssueID          string `json:"issue_id"`
	TriggerCommentID string `json:"trigger_comment_id"`
}

// Report is the dry-run output.
type Report struct {
	Domain               int            `json:"domain"`
	Counts               map[string]int `json:"counts"`
	Legacy               map[string]int `json:"legacy"`
	Unresolved           int            `json:"unresolved"`
	UnresolvedIssueIDs   []string       `json:"unresolved_issue_ids"`
	LegacyCommentSamples []Sample       `json:"legacy_comment_samples"`
	Changed              int            `json:"changed"`
}

func newReport() Report {
	return Report{
		Counts:               map[string]int{},
		Legacy:               map[string]int{},
		UnresolvedIssueIDs:   []string{},
		LegacyCommentSamples: []Sample{},
	}
}

// row is one issue of the domain with the facts the rules read.
type row struct {
	ID          pgtype.UUID
	WorkspaceID pgtype.UUID
	CreatedVia  pgtype.Text
	CreatorType string
	CreatorID   pgtype.UUID
	OriginType  pgtype.Text
	OriginID    pgtype.UUID
	ParentID    pgtype.UUID
	IsRelease   bool
	IsCIWatch   bool
}

// decision is the classifier's answer for one row.
type decision struct {
	row
	Value      string // the value to store; empty for unknown and unresolved
	Unknown    bool   // rule 7: stays NULL
	Unresolved bool   // rule 10: unchanged
	Legacy     bool
	Trigger    pgtype.UUID
}

func (d decision) bucket() string {
	if d.Unknown {
		return Unknown
	}
	return d.Value
}

// changes reports whether apply must write this row.
func (d decision) changes() bool {
	return !d.Unknown && !d.Unresolved && (!d.CreatedVia.Valid || d.CreatedVia.String != d.Value)
}

// decide applies rule v1 to one row. Platform create paths that have no row in
// the plan table (release issues, CI-watch issues) are checked before rules 8
// and 9: they store `autopilot` at create, so old rows must match.
func decide(ctx context.Context, q *db.Queries, r row) (decision, error) {
	d := decision{row: r}
	origin := r.OriginType.String
	switch r.CreatorType {
	case "member":
		if integrationOrigins[origin] {
			d.Unknown = true
			return d, nil
		}
		in := issuecreatedvia.Input{OriginType: origin}
		if origin != issuecreatedvia.OriginQuickCreate && origin != issuecreatedvia.OriginAutopilot && (r.IsRelease || r.IsCIWatch) {
			d.Value = issuecreatedvia.Autopilot
			return d, nil
		}
		d.Value = issuecreatedvia.Classify(in)
		return d, nil
	case "agent":
	default:
		d.Unresolved = true
		return d, nil
	}

	if origin == issuecreatedvia.OriginQuickCreate || origin == issuecreatedvia.OriginAutopilot {
		d.Value = issuecreatedvia.Classify(issuecreatedvia.Input{OriginType: origin, CreatorAgent: true})
		return d, nil
	}
	// Rule 10: no origin stamp, or the origin task row is gone.
	if origin != issuecreatedvia.OriginAgentCreate || !r.OriginID.Valid {
		d.Unresolved = true
		return d, nil
	}
	if _, err := q.GetAgentTask(ctx, r.OriginID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			d.Unresolved = true
			return d, nil
		}
		return d, fmt.Errorf("read origin task %x: %w", r.OriginID.Bytes, err)
	}
	in := issuecreatedvia.BuildInput(ctx, q, issuecreatedvia.Request{
		WorkspaceID:  r.WorkspaceID,
		CreatorAgent: true,
		AgentID:      r.CreatorID,
		TaskID:       r.OriginID,
		ParentID:     r.ParentID,
	})
	d.Value = issuecreatedvia.Classify(in)
	if in.Task != nil && in.Task.EvidenceKindNull && !in.Task.HasChatSession && (d.Value == issuecreatedvia.Comment || d.Value == issuecreatedvia.Agent) {
		d.Legacy = true
		d.Trigger = in.Task.TriggerCommentID
	}
	return d, nil
}

const domainSQL = `
SELECT i.id, i.workspace_id, i.created_via, i.creator_type, i.creator_id, i.origin_type, i.origin_id, i.parent_issue_id,
       EXISTS (SELECT 1 FROM github_release_deploy d WHERE d.issue_id = i.id),
       COALESCE(i.metadata ? 'ci_watch_count', false)
  FROM issue i
 WHERE (i.created_via IS NULL OR i.created_via = 'agent')
   AND ($1::uuid IS NULL OR i.workspace_id = $1)
   AND ($2::uuid IS NULL OR i.id > $2)
   AND (COALESCE(cardinality($4::uuid[]), 0) = 0 OR i.id = ANY($4))
 ORDER BY i.id
 LIMIT $3`

// scan walks the domain in id order, one batch at a time, and hands each
// batch's decisions to fn. A batch is fully read before fn runs, so fn may
// write without holding the read cursor open.
func scan(ctx context.Context, pool *pgxpool.Pool, opts Options, fn func([]decision) error) error {
	q := db.New(pool)
	var after pgtype.UUID
	for {
		rows, err := pool.Query(ctx, domainSQL, opts.WorkspaceID, after, opts.batch(), opts.IssueIDs)
		if err != nil {
			return fmt.Errorf("read domain: %w", err)
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.CreatedVia, &r.CreatorType, &r.CreatorID, &r.OriginType, &r.OriginID, &r.ParentID, &r.IsRelease, &r.IsCIWatch); err != nil {
				rows.Close()
				return fmt.Errorf("scan domain: %w", err)
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read domain: %w", err)
		}
		if len(batch) == 0 {
			return nil
		}
		decisions := make([]decision, 0, len(batch))
		for _, r := range batch {
			d, err := decide(ctx, q, r)
			if err != nil {
				return err
			}
			decisions = append(decisions, d)
		}
		if err := fn(decisions); err != nil {
			return err
		}
		after = batch[len(batch)-1].ID
	}
}

func (rep *Report) add(d decision) {
	rep.Domain++
	if d.Unresolved {
		rep.Unresolved++
		if len(rep.UnresolvedIssueIDs) < maxListed {
			rep.UnresolvedIssueIDs = append(rep.UnresolvedIssueIDs, uuidString(d.ID))
		}
		return
	}
	rep.Counts[d.bucket()]++
	if d.Legacy {
		rep.Legacy[d.Value]++
		if d.Value == issuecreatedvia.Comment && len(rep.LegacyCommentSamples) < maxSamples {
			rep.LegacyCommentSamples = append(rep.LegacyCommentSamples, Sample{IssueID: uuidString(d.ID), TriggerCommentID: uuidString(d.Trigger)})
		}
	}
	if d.changes() {
		rep.Changed++
	}
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// DryRun classifies the domain and writes nothing.
func DryRun(ctx context.Context, pool *pgxpool.Pool, opts Options) (Report, error) {
	rep := newReport()
	err := scan(ctx, pool, opts, func(ds []decision) error {
		for _, d := range ds {
			rep.add(d)
		}
		return nil
	})
	return rep, err
}

// ApplyResult is what Apply did.
type ApplyResult struct {
	Report
	Written int `json:"written"`
	// Skipped counts rows that changed under the run: their value was not the
	// one the run read, so nothing was written and no receipt line exists.
	Skipped int `json:"skipped"`
}

type receiptLine struct {
	IssueID string  `json:"issue_id"`
	Old     *string `json:"old"`
	New     string  `json:"new"`
}

// withLock holds the session advisory lock for the whole run. A second run
// fails at once instead of waiting.
func withLock(ctx context.Context, pool *pgxpool.Pool, fn func() error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire lock connection: %w", err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&got); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	if !got {
		return errors.New("another backfill_issue_created_via run holds the advisory lock")
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey) }()
	return fn()
}

// Apply writes the classified values in batches, one commit per batch, and
// appends one receipt line per changed row to receiptPath. The receipt file
// must not exist. Each batch's lines are written and synced before its commit,
// so a crash can leave a line for an uncommitted row (revert skips it) but
// never a committed row without a line.
func Apply(ctx context.Context, pool *pgxpool.Pool, opts Options, receiptPath string) (ApplyResult, error) {
	res := ApplyResult{Report: newReport()}
	out, err := os.OpenFile(receiptPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return res, fmt.Errorf("open receipt: %w", err)
	}
	defer out.Close()
	enc := json.NewEncoder(out)

	err = withLock(ctx, pool, func() error {
		return scan(ctx, pool, opts, func(ds []decision) error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(context.Background())
			var lines []receiptLine
			for _, d := range ds {
				res.Report.add(d)
				if !d.changes() {
					continue
				}
				tag, err := tx.Exec(ctx, `UPDATE issue SET created_via = $2 WHERE id = $1 AND created_via IS NOT DISTINCT FROM $3`,
					d.ID, d.Value, d.CreatedVia)
				if err != nil {
					return fmt.Errorf("update issue %s: %w", uuidString(d.ID), err)
				}
				if tag.RowsAffected() == 0 {
					res.Skipped++
					continue
				}
				l := receiptLine{IssueID: uuidString(d.ID), New: d.Value}
				if d.CreatedVia.Valid {
					old := d.CreatedVia.String
					l.Old = &old
				}
				lines = append(lines, l)
			}
			for _, l := range lines {
				if err := enc.Encode(l); err != nil {
					return fmt.Errorf("write receipt: %w", err)
				}
			}
			if err := out.Sync(); err != nil {
				return fmt.Errorf("sync receipt: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			res.Written += len(lines)
			return nil
		})
	})
	return res, err
}

// RevertResult is what Revert did.
type RevertResult struct {
	Reverted int `json:"reverted"`
	Skipped  int `json:"skipped"`
}

// Revert writes `old` back where the current value still equals `new`. A row
// edited after the apply, or a receipt line whose commit never happened, is
// skipped.
func Revert(ctx context.Context, pool *pgxpool.Pool, opts Options, receiptPath string) (RevertResult, error) {
	var res RevertResult
	in, err := os.Open(receiptPath)
	if err != nil {
		return res, fmt.Errorf("open receipt: %w", err)
	}
	defer in.Close()

	err = withLock(ctx, pool, func() error {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var batch []receiptLine
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(context.Background())
			n := 0
			for _, l := range batch {
				tag, err := tx.Exec(ctx, `UPDATE issue SET created_via = $2::text
					 WHERE id = $1::uuid AND created_via = $3 AND ($4::uuid IS NULL OR workspace_id = $4)
					 AND (COALESCE(cardinality($5::uuid[]), 0) = 0 OR id = ANY($5))`,
					l.IssueID, l.Old, l.New, opts.WorkspaceID, opts.IssueIDs)
				if err != nil {
					return fmt.Errorf("revert issue %s: %w", l.IssueID, err)
				}
				if tag.RowsAffected() == 1 {
					n++
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			res.Reverted += n
			res.Skipped += len(batch) - n
			batch = batch[:0]
			return nil
		}
		for sc.Scan() {
			var l receiptLine
			if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
				return fmt.Errorf("receipt line %q: %w", sc.Text(), err)
			}
			batch = append(batch, l)
			if len(batch) >= opts.batch() {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("read receipt: %w", err)
		}
		return flush()
	})
	return res, err
}
