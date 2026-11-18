package ghstack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ReportAction is the activity_log action of a stack report.
const ReportAction = "github_stack_report"

// Store is the database-backed ModeSource and Reporter. It reads settings and
// links fresh on every call and adds no table.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var (
	_ ModeSource = (*Store)(nil)
	_ Reporter   = (*Store)(nil)
)

// Resolve reads the settings of every workspace bound to the installation.
func (s *Store) Resolve(ctx context.Context, installationID int64, owner, repo string) (Resolution, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT w.id::text, w.settings
		   FROM github_installation gi JOIN workspace w ON w.id = gi.workspace_id
		  WHERE gi.installation_id = $1
		  ORDER BY gi.created_at, gi.id`, installationID)
	if err != nil {
		return Resolution{}, fmt.Errorf("read bound workspaces: %w", err)
	}
	defer rows.Close()
	slug := owner + "/" + repo
	var res Resolution
	var raws [][]byte
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return Resolution{}, fmt.Errorf("scan bound workspace: %w", err)
		}
		raws = append(raws, raw)
		res.Workspaces = append(res.Workspaces, WorkspaceMode{WorkspaceID: id, Mode: EffectiveMode(slug, raw)})
	}
	if err := rows.Err(); err != nil {
		return Resolution{}, fmt.Errorf("read bound workspaces: %w", err)
	}
	res.Final = FinalMode(slug, raws, EnvKill())
	if EnvKill() {
		res.Workspaces = nil // shutdown: no reports either
	}
	return res, nil
}

// Report writes one system activity entry on each issue linked to PR N, in
// each workspace of the outcome. A report identical to the issue's latest
// report for that PR is skipped; a changed one is written.
func (s *Store) Report(ctx context.Context, o Outcome) error {
	details, err := json.Marshal(map[string]any{
		"owner": o.Job.Owner, "repo": o.Job.Repo, "pr": o.Job.PR,
		"base_ref": o.BaseRef, "code": string(o.Code),
	})
	if err != nil {
		return err
	}
	var errs []string
	for _, w := range o.Workspaces {
		issues, err := s.linkedIssues(ctx, w.WorkspaceID, o.Job)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for _, issue := range issues {
			if err := s.insertIfChanged(ctx, w.WorkspaceID, issue, o, details); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("stack report: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (s *Store) linkedIssues(ctx context.Context, workspaceID string, j Job) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ipr.issue_id::text
		   FROM github_pull_request pr JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
		  WHERE pr.workspace_id = $1::uuid AND pr.repo_owner = $2 AND pr.repo_name = $3 AND pr.pr_number = $4`,
		workspaceID, j.Owner, j.Repo, j.PR)
	if err != nil {
		return nil, fmt.Errorf("list linked issues: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan linked issue: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// insertIfChanged is one statement: it inserts unless the latest report on the
// issue for this PR already carries the same base ref and code. The executor
// runs one job per repo at a time, so two writers do not race here.
func (s *Store) insertIfChanged(ctx context.Context, workspaceID, issueID string, o Outcome, details []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details)
		 SELECT $1::uuid, $2::uuid, 'system', $3, $4::jsonb
		  WHERE NOT EXISTS (
		    SELECT 1 FROM (
		      SELECT details FROM activity_log
		       WHERE issue_id = $2::uuid AND action = $3
		         AND details->>'owner' = $5 AND details->>'repo' = $6 AND details->>'pr' = $7
		       ORDER BY created_at DESC, id DESC LIMIT 1
		    ) last
		    WHERE last.details->>'base_ref' = $8 AND last.details->>'code' = $9)`,
		workspaceID, issueID, ReportAction, details,
		o.Job.Owner, o.Job.Repo, fmt.Sprint(o.Job.PR), o.BaseRef, string(o.Code))
	if err != nil {
		return fmt.Errorf("write report on issue %s: %w", issueID, err)
	}
	return nil
}

var _ Links = (*Store)(nil)

// IsLinked reports whether PR N is linked to a Multica issue in any workspace.
func (s *Store) IsLinked(ctx context.Context, j Job) (bool, error) {
	var linked bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM github_pull_request pr JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
		    WHERE pr.installation_id = $1 AND pr.repo_owner = $2 AND pr.repo_name = $3 AND pr.pr_number = $4)`,
		j.InstallationID, j.Owner, j.Repo, j.PR).Scan(&linked)
	if err != nil {
		return false, fmt.Errorf("check pull request link: %w", err)
	}
	return linked, nil
}

// ListLinkedStacked lists linked open PRs that may sit above another layer,
// after the given PR in key order. The default branch is not stored, so
// "non-default base" excludes the usual default names (main, master); any other
// base counts, whether or not its lower PR is mirrored here. A missed webhook
// must not hide a layer from the sweep: the executor finds the real base via
// GitHub and no-ops when the PR is not part of a stack.
func (s *Store) ListLinkedStacked(ctx context.Context, after Job, limit int) ([]Job, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT pr.installation_id, pr.repo_owner, pr.repo_name, pr.pr_number
		   FROM github_pull_request pr JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
		  WHERE pr.state IN ('open', 'draft')
		    AND pr.snapshot_base_ref IS NOT NULL
		    AND pr.snapshot_base_ref NOT IN ('main', 'master')
		    AND (pr.installation_id, pr.repo_owner, pr.repo_name, pr.pr_number) > ($1, $2, $3, $4)
		  ORDER BY pr.installation_id, pr.repo_owner, pr.repo_name, pr.pr_number
		  LIMIT $5`,
		after.InstallationID, after.Owner, after.Repo, after.PR, limit)
	if err != nil {
		return nil, fmt.Errorf("list linked stacked pull requests: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.InstallationID, &j.Owner, &j.Repo, &j.PR); err != nil {
			return nil, fmt.Errorf("scan linked stacked pull request: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
