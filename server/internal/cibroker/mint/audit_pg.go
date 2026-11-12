package mint

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGAudit writes audit rows to ci_broker_audit (migration 99993).
type PGAudit struct{ pool *pgxpool.Pool }

// NewPGAudit returns an AuditStore backed by pool.
func NewPGAudit(pool *pgxpool.Pool) *PGAudit { return &PGAudit{pool: pool} }

// Record inserts one row. Row has no token field, so none can be stored.
func (a *PGAudit) Record(ctx context.Context, r Row) error {
	perms, err := json.Marshal(r.Permissions)
	if err != nil {
		return err
	}
	_, err = a.pool.Exec(ctx, `INSERT INTO ci_broker_audit
		(at, repository_id, repository, run_id, run_attempt, workflow_ref, event, ref,
		 runner_environment, environment, permissions, result, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		r.At, r.RepositoryID, r.Repository, r.RunID, r.RunAttempt, r.WorkflowRef, r.Event, r.Ref,
		r.RunnerEnvironment, r.Environment, perms, r.Result, r.Reason)
	return err
}
