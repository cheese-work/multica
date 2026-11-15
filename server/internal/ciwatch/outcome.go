package ciwatch

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Outcome labels for ci_event observations (CHE-4265, Outcome labels).
// LabelSelfHealed lives in types.go.
const (
	LabelRerunGreen    = "rerun_green"
	LabelRerunRed      = "rerun_red"
	LabelFixedByPR     = "fixed_by_pr"
	LabelHumanOverride = "human_override"
)

const (
	flakeGreenRerun = "green_rerun"
	flakeShaFlips   = "sha_flips"
	reasonCap       = "cap"               // ledger cancel reason for a capped action
	healWindow      = 30 * 24 * time.Hour // ponytail: failures older than this are never healed-labelled
)

// Labeler sets outcome labels on ci_event observations and keeps the flake counter.
// A label is written once, except human_override replaces any label and a
// watcher rerun result replaces self_healed (another run's green got there first).
type Labeler struct {
	pool DB
	now  func() time.Time
}

// DB is the slice of a pgx pool the Labeler uses; the HTTP handler passes its own.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func NewLabeler(pool DB, now func() time.Time) *Labeler {
	if now == nil {
		now = time.Now
	}
	return &Labeler{pool: pool, now: now}
}

// RunCompleted labels earlier failures once run finished and updates the flake
// counter. Call it for every completed run (success or failure) after the
// workflow-run mirror row is written. Redelivery is a no-op.
func (l *Labeler) RunCompleted(ctx context.Context, run Run) error {
	now := l.now()
	if run.RunAttempt > 1 {
		if err := l.rerunCompleted(ctx, run, now); err != nil {
			return err
		}
	}
	if run.Conclusion == "success" {
		if err := l.healed(ctx, run, now); err != nil {
			return err
		}
	}
	return l.shaFlips(ctx, run, now)
}

// rerunCompleted handles attempt N>1: the watcher's own rerun gives
// rerun_green/rerun_red; any other rerun is a human acting (human_override).
func (l *Labeler) rerunCompleted(ctx context.Context, run Run, now time.Time) error {
	prev := run
	prev.RunAttempt--
	var id, class, sha, sig string
	var created time.Time
	err := l.pool.QueryRow(ctx, `SELECT id::text, class, created_at, coalesce(inputs->>'head_sha',''), coalesce(inputs->>'signature_hash','')
		FROM system1_observation WHERE workspace_id=$1 AND kind='ci_event' AND subject_id=$2`,
		run.WorkspaceID, subjectID(prev)).Scan(&id, &class, &created, &sha, &sig)
	if err != nil {
		return noRows(err) // no failure observation for the previous attempt: nothing to label
	}
	var ours bool
	if err := l.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ci_watch_action
		WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND original_run_id=$4 AND action_kind='rerun'
		  AND expected_attempt=$5 AND state IN ('executing','done','ambiguous'))`,
		run.WorkspaceID, run.RepoOwner, run.RepoName, run.RunID, run.RunAttempt).Scan(&ours); err != nil {
		return err
	}
	green := run.Conclusion == "success"
	var ttg *int64
	if green {
		ms := now.Sub(created).Milliseconds()
		ttg = &ms
	}
	label, override := LabelHumanOverride, true
	if ours {
		label, override = LabelRerunRed, false
		if green {
			label = LabelRerunGreen
		}
	}
	if err := l.apply(ctx, id, label, ttg, override, now); err != nil {
		return err
	}
	// Flake rule 1: green rerun of the same attempt, no new commit, class not rate-limit.
	if green && sig != "" && sha == run.HeadSHA && class != string(ClassRateLimit) &&
		(class == string(ClassTransient) || class == string(ClassCode) || class == string(ClassUnknown)) {
		return l.increment(ctx, run, sig, flakeGreenRerun, strconv.FormatInt(run.RunID, 10), now)
	}
	return nil
}

func (l *Labeler) apply(ctx context.Context, id, label string, ttg *int64, override bool, now time.Time) error {
	_, err := l.pool.Exec(ctx, `UPDATE system1_observation SET
		outcome_label = CASE WHEN $3::bool OR outcome_label IS NULL OR outcome_label = 'self_healed' THEN $2::text ELSE outcome_label END,
		time_to_green_ms = COALESCE(time_to_green_ms, $4::bigint),
		reversed_at = CASE WHEN $3::bool THEN COALESCE(reversed_at, $5::timestamptz) ELSE reversed_at END,
		labeled_at = COALESCE(labeled_at, $5::timestamptz)
		WHERE id=$1`, id, label, override, ttg, now)
	return err
}

// healed closes every open failure of the same workflow and branch with the
// time to this green run. fixed_by_pr when a PR linked to the signature's owner
// issue merged after the failure, else self_healed.
func (l *Labeler) healed(ctx context.Context, run Run, now time.Time) error {
	_, err := l.pool.Exec(ctx, `UPDATE system1_observation o SET
		outcome_label = COALESCE(o.outcome_label, CASE WHEN EXISTS (
			SELECT 1 FROM ci_signature_owner so
			JOIN issue_pull_request ip ON ip.issue_id = so.issue_id
			JOIN github_pull_request pr ON pr.id = ip.pull_request_id
			WHERE so.workspace_id = o.workspace_id AND so.repo_owner = $2 AND so.repo_name = $3
			  AND so.signature_hash = o.inputs->>'signature_hash'
			  AND pr.merged_at > o.created_at AND pr.merged_at <= $7::timestamptz
		) THEN $8::text ELSE $9::text END),
		time_to_green_ms = (extract(epoch FROM ($7::timestamptz - o.created_at)) * 1000)::bigint,
		labeled_at = COALESCE(o.labeled_at, $7::timestamptz)
		WHERE o.workspace_id=$1 AND o.kind='ci_event' AND o.time_to_green_ms IS NULL
		  AND o.created_at > $6::timestamptz AND o.created_at <= $7::timestamptz
		  AND o.inputs->>'repo' = $2 || '/' || $3 AND o.inputs->>'workflow_path' = $4 AND o.inputs->>'branch' = $5
		  AND coalesce(o.inputs->>'signature_hash','') <> ''`,
		run.WorkspaceID, run.RepoOwner, run.RepoName, run.WorkflowPath, run.HeadBranch,
		now.Add(-healWindow), now, LabelFixedByPR, LabelSelfHealed)
	return err
}

// shaFlips is flake rule 2: two or more pass/fail flips of one workflow on one
// head SHA add one increment per signature.
// ponytail: reads github_workflow_run, which keeps one row per run id (latest
// attempt), so flips inside a single run's attempts are not seen.
func (l *Labeler) shaFlips(ctx context.Context, run Run, now time.Time) error {
	if run.HeadSHA == "" {
		return nil
	}
	rows, err := l.pool.Query(ctx, `SELECT conclusion FROM github_workflow_run
		WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND workflow_path=$4 AND head_sha=$5
		  AND conclusion IN ('success','failure') ORDER BY created_at, run_id`,
		run.WorkspaceID, run.RepoOwner, run.RepoName, run.WorkflowPath, run.HeadSHA)
	if err != nil {
		return err
	}
	defer rows.Close()
	flips, prev := 0, ""
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return err
		}
		if prev != "" && c != prev {
			flips++
		}
		prev = c
	}
	if err := rows.Err(); err != nil || flips < 2 {
		return err
	}
	rows.Close()
	var sig string
	err = l.pool.QueryRow(ctx, `SELECT inputs->>'signature_hash' FROM system1_observation
		WHERE workspace_id=$1 AND kind='ci_event' AND class <> $5
		  AND inputs->>'repo' = $2 || '/' || $3 AND inputs->>'workflow_path' = $4 AND inputs->>'head_sha' = $6
		  AND coalesce(inputs->>'signature_hash','') <> ''
		ORDER BY created_at DESC LIMIT 1`,
		run.WorkspaceID, run.RepoOwner, run.RepoName, run.WorkflowPath, string(ClassRateLimit), run.HeadSHA).Scan(&sig)
	if err != nil {
		return noRows(err)
	}
	return l.increment(ctx, run, sig, flakeShaFlips, run.HeadSHA, now)
}

func (l *Labeler) increment(ctx context.Context, run Run, sig, reason, ref string, now time.Time) error {
	_, err := l.pool.Exec(ctx, `INSERT INTO ci_flake_increment (workspace_id, repo_owner, repo_name, signature_hash, reason, ref, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`,
		run.WorkspaceID, run.RepoOwner, run.RepoName, sig, reason, ref, now)
	return err
}

// Set labels the observation of run if it has no label yet (P0 rows: self_healed).
func (l *Labeler) Set(ctx context.Context, run Run, label string) error {
	_, err := l.pool.Exec(ctx, `UPDATE system1_observation SET outcome_label = COALESCE(outcome_label, $3::text),
		labeled_at = COALESCE(labeled_at, $4::timestamptz)
		WHERE workspace_id=$1 AND kind='ci_event' AND subject_id=$2`,
		run.WorkspaceID, subjectID(run), label, l.now())
	return err
}

// OverrideByIssue marks the still-open failures of the signature owned by
// issueID as human_override with reversed_at. Call it when a person takes over
// the owner issue.
func (l *Labeler) OverrideByIssue(ctx context.Context, workspaceID, issueID string) error {
	_, err := l.pool.Exec(ctx, `UPDATE system1_observation o SET outcome_label=$3::text,
		reversed_at = COALESCE(o.reversed_at, $4::timestamptz), labeled_at = COALESCE(o.labeled_at, $4::timestamptz)
		FROM ci_signature_owner so
		WHERE so.workspace_id=$1 AND so.issue_id=$2 AND o.workspace_id=$1 AND o.kind='ci_event'
		  AND o.time_to_green_ms IS NULL AND o.reversed_at IS NULL
		  AND o.inputs->>'repo' = so.repo_owner || '/' || so.repo_name
		  AND o.inputs->>'signature_hash' = so.signature_hash`,
		workspaceID, issueID, LabelHumanOverride, l.now())
	return err
}

// FlakeCount7d is the History.FlakeCount7d value for signature.
func (l *Labeler) FlakeCount7d(ctx context.Context, workspaceID, owner, repo, signature string) (int, error) {
	var n int
	err := l.pool.QueryRow(ctx, `SELECT count(*) FROM ci_flake_increment
		WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4 AND created_at > $5`,
		workspaceID, owner, repo, signature, l.now().Add(-7*24*time.Hour)).Scan(&n)
	return n, err
}

// Report is the daily shadow report for one workspace.
type Report struct {
	Events        int
	ClassMix      map[string]int
	ProtocolMix   map[string]int
	JevPredicted  int // events with a Jev prediction
	JevAgreed     int // of those, predicted key == rule choice
	Overrides     int
	Capped        int            // actions cancelled with reason cap
	Cancelled     map[string]int // all cancelled actions by reason
	TopSignatures []SignatureCount
}

type SignatureCount struct {
	Signature string
	Count     int
}

// ShadowReport summarises ci_event observations created in [from, to) and the
// watcher actions cancelled in the same window.
func (l *Labeler) ShadowReport(ctx context.Context, workspaceID string, from, to time.Time) (Report, error) {
	var r Report
	var err error
	const win = `workspace_id=$1 AND kind='ci_event' AND created_at >= $2 AND created_at < $3`
	if r.ClassMix, err = l.mix(ctx, `SELECT class, count(*) FROM system1_observation WHERE `+win+` GROUP BY 1`, workspaceID, from, to); err != nil {
		return r, err
	}
	for _, n := range r.ClassMix {
		r.Events += n
	}
	if r.ProtocolMix, err = l.mix(ctx, `SELECT coalesce(choice_key,''), count(*) FROM system1_observation WHERE `+win+` GROUP BY 1`, workspaceID, from, to); err != nil {
		return r, err
	}
	if err := l.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE predicted_key IS NOT NULL),
		count(*) FILTER (WHERE predicted_key = choice_key), count(*) FILTER (WHERE reversed_at IS NOT NULL)
		FROM system1_observation WHERE `+win, workspaceID, from, to).Scan(&r.JevPredicted, &r.JevAgreed, &r.Overrides); err != nil {
		return r, err
	}
	if r.Cancelled, err = l.mix(ctx, `SELECT coalesce(nullif(reason,''),'unspecified'), count(*) FROM ci_watch_action
		WHERE workspace_id=$1 AND state='cancelled' AND updated_at >= $2 AND updated_at < $3 GROUP BY 1`, workspaceID, from, to); err != nil {
		return r, err
	}
	r.Capped = r.Cancelled[reasonCap]
	rows, err := l.pool.Query(ctx, `SELECT inputs->>'signature_hash', count(*) c FROM system1_observation
		WHERE `+win+` AND coalesce(inputs->>'signature_hash','') <> '' GROUP BY 1 ORDER BY c DESC, 1 LIMIT 5`, workspaceID, from, to)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var s SignatureCount
		if err := rows.Scan(&s.Signature, &s.Count); err != nil {
			return r, err
		}
		r.TopSignatures = append(r.TopSignatures, s)
	}
	return r, rows.Err()
}

func (l *Labeler) mix(ctx context.Context, q string, args ...any) (map[string]int, error) {
	rows, err := l.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		m[k] = n
	}
	return m, rows.Err()
}

func noRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
