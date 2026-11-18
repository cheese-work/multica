package intakehold

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Loaded is what Load read: the issue's facts, the project's entry and the
// database clock at the read. Entry is nil when the rule has no say.
type Loaded struct {
	Issue Issue
	Entry *Entry
	Now   time.Time
}

// ReadField is the intake_hold field of GET /api/issues/{id}.
type ReadField struct {
	Source      string     `json:"source,omitempty"`
	Held        bool       `json:"held"`
	Owned       bool       `json:"owned"`
	Reason      string     `json:"reason"`
	EndsAt      time.Time  `json:"ends_at"`
	ExemptAfter *time.Time `json:"exempt_after,omitempty"`
}

// Field is the read field of the issue, or nil when the rule has no say.
func (loaded Loaded) Field() *ReadField {
	verdict, applies := Evaluate(loaded.Issue, loaded.Entry, loaded.Now)
	if !applies {
		return nil
	}
	return &ReadField{Source: loaded.Entry.Source, Held: verdict.Held, Owned: verdict.Owned, Reason: verdict.Reason, EndsAt: loaded.Entry.EndsAt, ExemptAfter: verdict.ExemptAfter}
}

// Load reads the facts of one issue. It stops after the first query when there
// is no entry for the issue's project or the issue has a parent, so an
// unconfigured workspace pays one read. `now` is the database clock, the same
// clock every stored time of the rule comes from.
func Load(ctx context.Context, q db.DBTX, issueID string) (Loaded, error) {
	var (
		loaded   Loaded
		project  string
		origin   string
		originID string
		stored   string
	)
	issue := &loaded.Issue
	err := q.QueryRow(ctx, `
		SELECT i.id::text, i.title, i.status, i.priority, i.created_at, i.parent_issue_id IS NOT NULL,
		       COALESCE(i.project_id::text, ''), COALESCE(i.origin_type, ''), COALESCE(i.origin_id::text, ''),
		       COALESCE(w.settings -> 'intake_hold', 'null'::jsonb)::text, clock_timestamp()
		FROM issue i JOIN workspace w ON w.id = i.workspace_id
		WHERE i.id = $1`, issueID).Scan(&issue.ID, &issue.Title, &issue.Status, &issue.Priority, &issue.CreatedAt, &issue.HasParent, &project, &origin, &originID, &stored, &loaded.Now)
	if err != nil {
		return Loaded{}, err
	}
	if project == "" || issue.HasParent {
		return loaded, nil
	}
	if loaded.Entry = ParseStored([]byte(stored), project); loaded.Entry == nil {
		return loaded, nil
	}

	if issue.Labels, err = column(ctx, q, `SELECT l.name FROM issue_to_label il JOIN issue_label l ON l.id = il.label_id WHERE il.issue_id = $1`, issueID); err != nil {
		return Loaded{}, err
	}
	if origin == "agent_create" {
		// An autopilot-made issue has origin agent_create; its origin_id is the task, and the task carries the run.
		ids, err := column(ctx, q, `SELECT r.autopilot_id::text FROM agent_task_queue t JOIN autopilot_run r ON r.id = t.autopilot_run_id WHERE t.id = $1`, originID)
		if err != nil {
			return Loaded{}, err
		}
		if len(ids) > 0 {
			issue.AutopilotID = ids[0]
		}
	}
	if err = q.QueryRow(ctx, `SELECT prior_status, to_status FROM issue_decision_park WHERE issue_id = $1`, issueID).Scan(&issue.ParkPrior, &issue.ParkTo); err == nil {
		issue.HasPark = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Loaded{}, err
	}
	if issue.Rows, err = statusRows(ctx, q, issueID); err != nil {
		return Loaded{}, err
	}
	if issue.Requests, err = requests(ctx, q, issueID); err != nil {
		return Loaded{}, err
	}
	return loaded, nil
}

func column(ctx context.Context, q db.DBTX, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

func statusRows(ctx context.Context, q db.DBTX, issueID string) ([]StatusRow, error) {
	rows, err := q.Query(ctx, `SELECT COALESCE(actor_type, ''), details FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatusRow
	for rows.Next() {
		var (
			actor   string
			details []byte
		)
		if err := rows.Scan(&actor, &details); err != nil {
			return nil, err
		}
		var parsed struct {
			From          string `json:"from"`
			To            string `json:"to"`
			IntakeRelease string `json:"intake_release"`
			Trigger       struct {
				Cause      struct{ Kind string } `json:"cause"`
				AuthSource string                `json:"auth_source"`
			} `json:"trigger"`
		}
		if json.Unmarshal(details, &parsed) != nil {
			continue // a row this rule cannot read releases nothing
		}
		out = append(out, StatusRow{ActorType: actor, CauseKind: parsed.Trigger.Cause.Kind, AuthSource: parsed.Trigger.AuthSource, From: parsed.From, To: parsed.To, Release: parsed.IntakeRelease})
	}
	return out, rows.Err()
}

func requests(ctx context.Context, q db.DBTX, issueID string) ([]Request, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id::text, r.authority, r.escalation_class, r.status,
		       a.answered_by_user_id IS NOT NULL, a.wake_at, a.disclaimed_at IS NOT NULL
		FROM decision_request r LEFT JOIN decision_answer a ON a.decision_request_id = r.id
		WHERE r.issue_id = $1 AND r.authority = 'human' AND r.status = 'answered'
		ORDER BY r.created_at, r.id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var (
		out   []Request
		index = map[string]int{}
	)
	for rows.Next() {
		var (
			id      string
			request Request
			answer  Answer
		)
		if err := rows.Scan(&id, &request.Authority, &request.Class, &request.Status, &answer.ByPerson, &answer.WakeAt, &answer.Disclaimed); err != nil {
			return nil, err
		}
		at, seen := index[id]
		if !seen {
			at = len(out)
			index[id] = at
			out = append(out, request)
		}
		out[at].Answers = append(out[at].Answers, answer)
	}
	return out, rows.Err()
}
