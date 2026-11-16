package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Dispatch rules for merge_eligibility runs (CHE-5122). Being eligible only
// decides whether an agent run is worth starting; it never authorizes a merge.
// The merger keeps its own live checks and --match-head-commit.
//
// A candidate is one PR on one head and base, under one policy revision and one
// piece of review evidence. It is named by the "candidate" key every observed
// pull request carries.

func isMergeEligibilityCondition(raw []byte) bool {
	var c WakeupCondition
	return len(raw) > 0 && json.Unmarshal(raw, &c) == nil && c.Type == "merge_eligibility"
}

func (s *IssueWakeupService) mergeFactsSource() MergeFactsSource {
	if s.MergeFacts != nil {
		return s.MergeFacts
	}
	if s.Tasks != nil {
		return s.Tasks.MergeFacts
	}
	return nil
}

// observedMergeCandidates lists the candidate keys of an observed-facts map or
// of its JSON form.
func observedMergeCandidates(pullRequests any) []string {
	var keys []string
	switch prs := pullRequests.(type) {
	case []map[string]any:
		for _, pr := range prs {
			if k, _ := pr["candidate"].(string); k != "" {
				keys = append(keys, k)
			}
		}
	case []any:
		for _, p := range prs {
			if pr, ok := p.(map[string]any); ok {
				if k, _ := pr["candidate"].(string); k != "" {
					keys = append(keys, k)
				}
			}
		}
	}
	return keys
}

// taskMergeCandidates reads the candidates a queued run was created for from
// its stored evidence. A run whose evidence names none counts as stale.
func taskMergeCandidates(taskContext []byte) []string {
	var stored struct {
		Evidence wakeupEvidence `json:"wakeup_evidence"`
	}
	if json.Unmarshal(taskContext, &stored) != nil {
		return nil
	}
	var keys []string
	for _, fact := range stored.Evidence.Facts {
		var payload struct {
			Observed struct {
				PullRequests []any `json:"pull_requests"`
			} `json:"observed"`
		}
		if json.Unmarshal(fact.Payload, &payload) == nil {
			keys = append(keys, observedMergeCandidates(payload.Observed.PullRequests)...)
		}
	}
	return keys
}

func anyIn(keys []string, set map[string]bool) bool {
	for _, k := range keys {
		if set[k] {
			return true
		}
	}
	return false
}

// withdrawStaleMergeRuns cancels this rule's queued runs that have not started
// and whose candidates are all gone from the eligible set: a head or base
// changed, evidence was revoked, a pause was set, or the state is unknown now.
// A started run is never touched. The caller publishes the returned tasks
// after the transaction commits.
func withdrawStaleMergeRuns(ctx context.Context, tx pgx.Tx, q *db.Queries, w db.IssueWakeup, eligible []string) ([]db.AgentTaskQueue, error) {
	rows, err := tx.Query(ctx, `SELECT id, context FROM agent_task_queue
		WHERE context->>'wakeup_id'=$1 AND status IN ('queued','deferred') AND started_at IS NULL FOR UPDATE`, util.UUIDToString(w.ID))
	if err != nil {
		return nil, err
	}
	var stale []pgtype.UUID
	keep := map[string]bool{}
	for _, k := range eligible {
		keep[k] = true
	}
	for rows.Next() {
		var id pgtype.UUID
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		if !anyIn(taskMergeCandidates(raw), keep) {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []db.AgentTaskQueue
	for _, id := range stale {
		if _, err := tx.Exec(ctx, `UPDATE agent_task_queue SET status='cancelled',completed_at=now(),
			error='Merge candidate is no longer eligible' WHERE id=$1`, id); err != nil {
			return nil, err
		}
		task, err := q.GetAgentTask(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, nil
}

// dedupeMergeCandidates drops from observed every candidate that already has a
// queued or active run in the workspace, from this rule or any other. Each
// candidate is locked until the transaction ends, so two rules deciding at once
// cannot both queue it. It returns nil when nothing is left to dispatch.
func dedupeMergeCandidates(ctx context.Context, tx pgx.Tx, w db.IssueWakeup, observed map[string]any) (map[string]any, error) {
	prs, _ := observed["pull_requests"].([]map[string]any)
	sort.Slice(prs, func(i, j int) bool { return fmt.Sprint(prs[i]["candidate"]) < fmt.Sprint(prs[j]["candidate"]) })
	var fresh []map[string]any
	for _, pr := range prs {
		key, _ := pr["candidate"].(string)
		if key == "" {
			continue
		}
		scope := util.UUIDToString(w.WorkspaceID) + "/" + key
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "merge-candidate:"+scope); err != nil {
			return nil, err
		}
		var live bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM agent_task_queue t JOIN issue_wakeup o ON o.id::text = t.context->>'wakeup_id'
			WHERE o.workspace_id=$1 AND o.condition->>'type'='merge_eligibility'
			  AND t.status IN ('queued','deferred','dispatched','running','waiting_local_directory')
			  AND jsonb_path_exists(t.context, '$.wakeup_evidence.facts[*].payload.observed.pull_requests[*] ? (@.candidate == $key)', jsonb_build_object('key',$2::text)))`,
			w.WorkspaceID, key).Scan(&live)
		if err != nil {
			return nil, err
		}
		if !live {
			fresh = append(fresh, pr)
		}
	}
	if len(fresh) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(observed))
	for k, v := range observed {
		out[k] = v
	}
	out["pull_requests"] = fresh
	return out, nil
}

// checkMergeClaim judges the run's candidate again, against current stored
// state, when the agent claims or starts it. It fails closed: a candidate that
// is gone, or whose state is unknown, does not run.
func (s *IssueWakeupService) checkMergeClaim(ctx context.Context, task db.AgentTaskQueue, w db.IssueWakeup) error {
	if s.Tasks == nil || s.Tasks.TxStarter == nil {
		return ErrWakeupForbidden
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, _, observed, err := s.evaluateMergeEligibility(ctx, tx, s.Tasks.Queries.WithTx(tx), w)
	if err != nil {
		if conditionDiagnostic(err) {
			return ErrWakeupForbidden
		}
		return err
	}
	keep := map[string]bool{}
	for _, k := range observedMergeCandidates(observed["pull_requests"]) {
		keep[k] = true
	}
	if !anyIn(taskMergeCandidates(task.Context), keep) {
		return errors.Join(ErrWakeupForbidden, errors.New("merge candidate is no longer eligible"))
	}
	return nil
}

// mergeCandidateKey is comma-free because conditionKeys splits fingerprints on commas.
func mergeCandidateKey(owner, repo string, number int32, head, base, policyRevision, evidence string) string {
	return strings.ReplaceAll(fmt.Sprintf("%s/%s#%d@%s>%s|%s|%s", owner, repo, number, head, base, policyRevision, evidence), ",", "%2C")
}
