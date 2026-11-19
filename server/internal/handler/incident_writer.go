package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issueposition"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// IncidentRow is one signal firing on a stopped issue (CHE-5738). Every dev-stop
// signal calls RecordIncident when it sees the stop and CloseIncident when the
// issue leaves the stop state.
type IncidentRow struct {
	Kind     string // signal kind; with the stopped issue it is the cause key
	Area     string // the title's area: incident(<area>): ...
	Impact   string // one line: what stopped
	Row      string // timeline row text, appended when the cause already has an incident
	EventKey string // the same key twice appends one row
	At       time.Time
}

func ictStamp(t time.Time) string { return t.In(ictLocation).Format("2006-01-02 15:04") + " ICT" }

func stopDuration(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

// RecordIncident opens an incident for the cause key (stopped issue, kind), or
// appends a timeline row to the one already open for it.
func (h *Handler) RecordIncident(ctx context.Context, issue db.Issue, r IncidentRow) error {
	var created *db.Issue
	var row *db.Comment
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		qtx := h.Queries.WithTx(tx)
		key := uuidToString(issue.WorkspaceID) + "/" + uuidToString(issue.ID) + "/incident/" + r.Kind
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return err
		}
		var causeID, incidentID pgtype.UUID
		var last string
		err := tx.QueryRow(ctx, `SELECT id, incident_issue_id, last_event_key FROM incident_cause
			WHERE workspace_id=$1 AND issue_id=$2 AND kind=$3 AND closed_at IS NULL FOR UPDATE`,
			issue.WorkspaceID, issue.ID, r.Kind).Scan(&causeID, &incidentID, &last)
		switch {
		case err == nil:
			if last == r.EventKey {
				return nil
			}
			if row, err = h.addIncidentRow(ctx, qtx, issue.WorkspaceID, incidentID, ictStamp(r.At)+": "+r.Row); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE incident_cause SET last_event_key=$2 WHERE id=$1`, causeID, r.EventKey)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		inc, err := h.createIncidentIssue(ctx, tx, qtx, issue, r)
		if err != nil {
			return err
		}
		created = &inc
		_, err = tx.Exec(ctx, `INSERT INTO incident_cause (id, workspace_id, issue_id, kind, incident_issue_id, last_event_key, opened_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, dbid.NewV7(), issue.WorkspaceID, issue.ID, r.Kind, inc.ID, r.EventKey, r.At)
		return err
	})
	if err != nil {
		return err
	}
	ws := uuidToString(issue.WorkspaceID)
	if created != nil {
		h.publish(protocol.EventIssueCreated, ws, "system", "", map[string]any{"issue": issueToResponse(*created, h.getIssuePrefix(ctx, issue.WorkspaceID))})
	}
	if row != nil {
		h.publishIncidentRow(ctx, *row)
	}
	return nil
}

// CloseIncident writes the close row, with the stop duration, on the open
// incident for the cause key and closes the cause. It does nothing when none is open.
func (h *Handler) CloseIncident(ctx context.Context, issue db.Issue, kind string, at time.Time) error {
	var row *db.Comment
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		var causeID, incidentID pgtype.UUID
		var opened time.Time
		err := tx.QueryRow(ctx, `SELECT id, incident_issue_id, opened_at FROM incident_cause
			WHERE workspace_id=$1 AND issue_id=$2 AND kind=$3 AND closed_at IS NULL FOR UPDATE`,
			issue.WorkspaceID, issue.ID, kind).Scan(&causeID, &incidentID, &opened)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if row, err = h.addIncidentRow(ctx, h.Queries.WithTx(tx), issue.WorkspaceID, incidentID,
			fmt.Sprintf("%s: stop ended after %s.", ictStamp(at), stopDuration(at.Sub(opened)))); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE incident_cause SET closed_at=$2 WHERE id=$1`, causeID, at)
		return err
	})
	if err == nil && row != nil {
		h.publishIncidentRow(ctx, *row)
	}
	return err
}

func (h *Handler) addIncidentRow(ctx context.Context, q *db.Queries, ws, incidentID pgtype.UUID, text string) (*db.Comment, error) {
	created, err := q.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: incidentID, WorkspaceID: ws,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Content: text, Type: "system",
	})
	if err != nil {
		return nil, err
	}
	c := created.Comment()
	return &c, nil
}

func (h *Handler) publishIncidentRow(ctx context.Context, c db.Comment) {
	h.publish(protocol.EventCommentCreated, uuidToString(c.WorkspaceID), "system", "", map[string]any{"comment": commentToResponse(c, nil, nil)})
}

// createIncidentIssue opens the incident: impact, ICT start, the stopped
// issue as source, and the owner. The owner is the person who asked for the
// stopped issue; an issue with no requester opens unassigned.
func (h *Handler) createIncidentIssue(ctx context.Context, tx pgx.Tx, q *db.Queries, src db.Issue, r IncidentRow) (db.Issue, error) {
	ws := src.WorkspaceID
	number, err := service.AllocateIssueNumber(ctx, q, ws, service.ResolveIssueCountPolicy(ctx, h.Entitlements, ws))
	if err != nil {
		return db.Issue{}, err
	}
	position, err := issueposition.NextTopPosition(ctx, tx, ws, "todo")
	if err != nil {
		return db.Issue{}, err
	}
	owner := "unassigned"
	var assignee pgtype.Text
	if src.RequestedByID.Valid {
		if u, err := q.GetUser(ctx, src.RequestedByID); err == nil {
			owner = u.Name
		}
		assignee = pgtype.Text{String: "member", Valid: true}
	}
	ref := fmt.Sprintf("%s-%d", h.getIssuePrefix(ctx, ws), src.Number)
	body := fmt.Sprintf("**Impact:** %s\n**Start:** %s\n**Source:** [%s](mention://issue/%s)\n**Owner:** %s\n",
		r.Impact, ictStamp(r.At), ref, uuidToString(src.ID), owner)
	return q.CreateIssueWithOrigin(ctx, db.CreateIssueWithOriginParams{
		ID: dbid.NewV7(), WorkspaceID: ws,
		Title:       fmt.Sprintf("incident(%s): %s %s", r.Area, ref, r.Impact),
		Description: pgtype.Text{String: body, Valid: true}, Status: "todo", Priority: "high",
		AssigneeType: assignee, AssigneeID: src.RequestedByID,
		CreatorType: src.CreatorType, CreatorID: src.CreatorID,
		Position: position, Number: number, ProjectID: src.ProjectID,
	})
}

// closeIncidentsOnStatusMove ends every open stop of an issue that moved to a
// status a dev stop does not hold it in. The move into the stop (the budget
// block) is the other way round and closes nothing.
func (h *Handler) closeIncidentsOnStatusMove(e events.Event) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	changed, _ := payload["status_changed"].(bool)
	resp, ok := payload["issue"].(IssueResponse)
	if !ok || !changed || resp.Status == issuestatus.Blocked || resp.Status == "need_decision" {
		return
	}
	ctx := context.Background()
	issue, err := h.Queries.GetIssue(ctx, parseUUID(resp.ID))
	if err != nil {
		return
	}
	var kinds []string
	err = h.withPgxTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind FROM incident_cause WHERE workspace_id=$1 AND issue_id=$2 AND closed_at IS NULL`, issue.WorkspaceID, issue.ID)
		if err != nil {
			return err
		}
		kinds, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		slog.Warn("incident: list open causes failed", "err", err, "issue_id", resp.ID)
		return
	}
	for _, kind := range kinds {
		if err := h.CloseIncident(ctx, issue, kind, time.Now()); err != nil {
			slog.Warn("incident: close failed", "err", err, "issue_id", resp.ID, "kind", kind)
		}
	}
}
