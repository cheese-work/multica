package intakehold

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Fix and ReleasePaths are the typed text of the 409 body (CHE-6911 E1).
const Fix = "create or keep the issue in backlog"

var ReleasePaths = []string{
	"a person moves the issue out of backlog to another status",
	"the issue ID is added to exempt_issue_ids in the intake_hold setting",
}

// HoldError refuses a write that is not a person's. EndsAt is the entry's end.
type HoldError struct {
	EndsAt time.Time
	Source string
}

func (e *HoldError) Error() string {
	return fmt.Sprintf("intake hold until %s: %s", e.EndsAt.Format(time.RFC3339), Fix)
}

// Enforce turns a verdict into the write's outcome. A held issue in mode block
// returns *HoldError; mode warn logs one line and refuses nothing. An exempt
// pass is logged. `what` names the write ("status" or "create").
func Enforce(loaded Loaded, what, actorType, actorID string) error {
	verdict, applies := Evaluate(loaded.Issue, loaded.Entry, loaded.Now)
	if !applies {
		return nil
	}
	attrs := []any{"issue_id", loaded.Issue.ID, "write", what, "actor_type", actorType, "actor_id", actorID, "source", loaded.Entry.Source, "reason", verdict.Reason}
	switch {
	case strings.HasPrefix(verdict.Reason, "exempt_"):
		slog.Info("intake_hold_exempt", attrs...)
	case verdict.Held && loaded.Entry.Mode == ModeWarn:
		slog.Warn("intake_hold_warn", attrs...)
	case verdict.Held:
		slog.Warn("intake_hold", attrs...)
		return &HoldError{EndsAt: loaded.Entry.EndsAt, Source: loaded.Entry.Source}
	}
	return nil
}

// CreateFacts is what a create states about the issue that does not exist yet.
type CreateFacts struct {
	WorkspaceID, ProjectID, Title string
	HasParent                     bool
	LabelIDs                      []string
	OriginType, OriginID          string
}

// LoadCreate reads the entry and the clock for a new issue. The issue is
// created now, in backlog, so conditions 1 to 3 and 6 decide it.
func LoadCreate(ctx context.Context, q db.DBTX, facts CreateFacts) (Loaded, error) {
	var (
		loaded Loaded
		stored string
	)
	err := q.QueryRow(ctx, `SELECT COALESCE(settings -> 'intake_hold', 'null'::jsonb)::text, clock_timestamp() FROM workspace WHERE id = $1`, facts.WorkspaceID).Scan(&stored, &loaded.Now)
	if err != nil {
		return Loaded{}, err
	}
	issue := &loaded.Issue
	*issue = Issue{Title: facts.Title, Status: "backlog", CreatedAt: loaded.Now, HasParent: facts.HasParent}
	if facts.ProjectID == "" || facts.HasParent {
		return loaded, nil
	}
	if loaded.Entry = ParseStored([]byte(stored), facts.ProjectID); loaded.Entry == nil {
		return loaded, nil
	}
	if len(facts.LabelIDs) > 0 {
		if issue.Labels, err = column(ctx, q, `SELECT name FROM issue_label WHERE id::text = ANY($1)`, facts.LabelIDs); err != nil {
			return Loaded{}, err
		}
	}
	if issue.AutopilotID, err = autopilotOf(ctx, q, facts.OriginType, facts.OriginID); err != nil {
		return Loaded{}, err
	}
	return loaded, nil
}

// ForCreate decides the status a create lands in and the release marker. For a
// held issue in mode block a create that names no status lands in backlog and
// one that names another status but cancelled is refused; mode warn writes one
// line and changes nothing. The marker is the entry source, for an issue that
// lands in backlog under the hold; the rule never reads it.
func ForCreate(loaded Loaded, status string, omitted bool, actorType, actorID string) (landing, marker string, err error) {
	landing = status
	verdict, applies := Evaluate(loaded.Issue, loaded.Entry, loaded.Now)
	if !applies || !verdict.Held {
		return landing, "", Enforce(loaded, "create", actorType, actorID)
	}
	if omitted && loaded.Entry.Mode == ModeBlock {
		landing = "backlog"
	}
	if landing != "backlog" && landing != "cancelled" {
		err = Enforce(loaded, "create", actorType, actorID)
	}
	if landing == "backlog" {
		marker = loaded.Entry.Source
	}
	return landing, marker, err
}
