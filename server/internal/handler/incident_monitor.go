package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/incident"
	"github.com/multica-ai/multica/server/internal/issuecreatedvia"
	"github.com/multica-ai/multica/server/internal/issueposition"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Provider incident monitor (CHE-9892). Off until a workspace sets
// settings.incident_response = {"enabled":true,"project_id":"<uuid>","notify_user_id":"<member user uuid>"}.
// It runs in the server, not as an agent run: a provider outage must not stop the
// check that reports it. State lives on the incident issue's metadata; no table.

const (
	incidentInterval = 10 * time.Minute
	// Enough history for the 30 minute window plus the 30 minute close timer.
	incidentLookback = 60 * time.Minute
	incidentMaxRuns  = 500
)

type incidentSettings struct {
	Enabled      bool   `json:"enabled"`
	ProjectID    string `json:"project_id"`
	NotifyUserID string `json:"notify_user_id"`
}

// RunIncidentMonitor sweeps every interval until ctx ends. interval <= 0 disables it.
func (h *Handler) RunIncidentMonitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.SweepIncidents(ctx, time.Now())
		}
	}
}

// SweepIncidents runs one pass over every workspace that enabled the monitor.
func (h *Handler) SweepIncidents(ctx context.Context, now time.Time) {
	rows, err := h.DB.Query(ctx, `SELECT id, settings->'incident_response' FROM workspace
		WHERE settings->'incident_response'->>'enabled' = 'true'`)
	if err != nil {
		slog.Warn("incident monitor: list workspaces", "error", err)
		return
	}
	type ws struct {
		id  pgtype.UUID
		cfg incidentSettings
	}
	var list []ws
	for rows.Next() {
		var w ws
		var raw []byte
		if err := rows.Scan(&w.id, &raw); err == nil && json.Unmarshal(raw, &w.cfg) == nil {
			list = append(list, w)
		}
	}
	rows.Close()
	for _, w := range list {
		if err := h.sweepWorkspaceIncidents(ctx, w.id, w.cfg, now); err != nil {
			slog.Warn("incident monitor: workspace sweep", "workspace_id", util.UUIDToString(w.id), "error", err)
		}
	}
}

func (h *Handler) sweepWorkspaceIncidents(ctx context.Context, wsID pgtype.UUID, cfg incidentSettings, now time.Time) error {
	notifier, err := util.ParseUUID(cfg.NotifyUserID)
	if err != nil {
		return fmt.Errorf("incident_response.notify_user_id: %w", err)
	}
	recent, err := h.incidentRecentRuns(ctx, wsID, now)
	if err != nil {
		return err
	}
	open, err := h.incidentOpen(ctx, wsID)
	if err != nil {
		return err
	}
	names := h.incidentNames(ctx, wsID)
	issueOf := map[incident.Class]string{}
	for _, o := range open {
		issueOf[o.Class] = o.IssueID
	}
	icfg := incident.DefaultConfig()
	var firstErr error
	for _, a := range incident.Plan(open, recent, now, icfg) {
		if a.IssueID == "" {
			a.IssueID = issueOf[a.Class] // the notice that follows an open in the same pass
		}
		var err error
		switch a.Kind {
		case incident.ActOpen:
			var id string
			if id, err = h.incidentOpenIssue(ctx, wsID, cfg, notifier, *a.Cluster, names); err == nil {
				issueOf[a.Class] = id
			}
		case incident.ActComment:
			err = h.incidentComment(ctx, wsID, notifier, a.IssueID, incident.CommentBody(a.NewRuns, names), a.NewRuns, false)
		case incident.ActNotify:
			if a.IssueID != "" {
				err = h.incidentNotify(ctx, wsID, notifier, a.IssueID, a.Class, recent, names, icfg, now)
			}
		case incident.ActClose:
			err = h.incidentComment(ctx, wsID, notifier, a.IssueID, incident.CloseBody(lastOf(open, a.IssueID), len(a.RetryRunIDs)), nil, true)
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s %s: %w", a.Kind, a.Class, err)
		}
	}
	return firstErr
}

func lastOf(open []incident.Open, issueID string) time.Time {
	for _, o := range open {
		if o.IssueID == issueID {
			return o.LastFailureAt
		}
	}
	return time.Time{}
}

func (h *Handler) incidentRecentRuns(ctx context.Context, wsID pgtype.UUID, now time.Time) ([]incident.Run, error) {
	rows, err := h.DB.Query(ctx, `SELECT t.id::text, t.agent_id::text, COALESCE(t.runtime_id::text,''), t.issue_id::text,
			COALESCE(t.failure_reason,''), COALESCE(t.error,''), COALESCE(t.completed_at, t.created_at)
		FROM agent_task_queue t JOIN agent a ON a.id = t.agent_id
		WHERE a.workspace_id = $1 AND t.status = 'failed' AND t.issue_id IS NOT NULL
		  AND COALESCE(t.completed_at, t.created_at) >= $2`, wsID, now.Add(-incidentLookback))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.Run
	for rows.Next() {
		var r incident.Run
		var reason string
		if err := rows.Scan(&r.ID, &r.AgentID, &r.RuntimeID, &r.IssueID, &reason, &r.Error, &r.FailedAt); err != nil {
			return nil, err
		}
		if c, ok := incident.ClassOf(reason, r.Error); ok {
			r.Class = c
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

func (h *Handler) incidentOpen(ctx context.Context, wsID pgtype.UUID) ([]incident.Open, error) {
	rows, err := h.DB.Query(ctx, `SELECT id::text, metadata FROM issue
		WHERE workspace_id = $1 AND metadata->>'incident_state' = 'open' AND status NOT IN ('done','cancelled')`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.Open
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var m struct {
			Class    string   `json:"incident_class"`
			Last     string   `json:"incident_last_failure_at"`
			Notified bool     `json:"incident_notified"`
			RunIDs   []string `json:"incident_run_ids"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Class == "" {
			continue
		}
		last, _ := time.Parse(time.RFC3339Nano, m.Last)
		out = append(out, incident.Open{IssueID: id, Class: incident.Class(m.Class), LastFailureAt: last, Notified: m.Notified, RunIDs: m.RunIDs})
	}
	return out, rows.Err()
}

// incidentNames resolves agent and issue ids once per pass.
func (h *Handler) incidentNames(ctx context.Context, wsID pgtype.UUID) incident.Names {
	agents := map[string]string{}
	if rows, err := h.DB.Query(ctx, `SELECT id::text, name FROM agent WHERE workspace_id = $1`, wsID); err == nil {
		for rows.Next() {
			var id, name string
			if rows.Scan(&id, &name) == nil {
				agents[id] = name
			}
		}
		rows.Close()
	}
	prefix := h.getIssuePrefix(ctx, wsID)
	issues := map[string]string{}
	return func(kind, id string) string {
		switch kind {
		case "agent":
			return agents[id]
		case "issue":
			if v, ok := issues[id]; ok {
				return v
			}
			var n int32
			if h.DB.QueryRow(ctx, `SELECT number FROM issue WHERE id = $1::uuid`, id).Scan(&n) == nil {
				issues[id] = fmt.Sprintf("[%s-%d](mention://issue/%s)", prefix, n, id)
			}
			return issues[id]
		}
		return ""
	}
}

func (h *Handler) incidentOpenIssue(ctx context.Context, wsID pgtype.UUID, cfg incidentSettings, creator pgtype.UUID, c incident.Cluster, names incident.Names) (string, error) {
	project, err := util.ParseUUID(cfg.ProjectID)
	if err != nil {
		return "", fmt.Errorf("incident_response.project_id: %w", err)
	}
	ids := make([]string, 0, len(c.Runs))
	for _, r := range c.Runs {
		ids = append(ids, r.ID)
	}
	meta, _ := json.Marshal(map[string]any{
		"incident_class": string(c.Class), "incident_state": "open", "incident_notified": false,
		"incident_last_failure_at": c.Last.UTC().Format(time.RFC3339Nano), "incident_run_ids": capIDs(ids),
	})
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := h.Queries.WithTx(tx)
	// One open incident per class: serialize concurrent sweeps (two servers) on the class.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, util.UUIDToString(wsID)+"/incident/"+string(c.Class)); err != nil {
		return "", err
	}
	var existing string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM issue WHERE workspace_id = $1 AND metadata->>'incident_state' = 'open'
		AND metadata->>'incident_class' = $2 AND status NOT IN ('done','cancelled') LIMIT 1`, wsID, string(c.Class)).Scan(&existing); err == nil {
		return existing, nil
	} else if err != pgx.ErrNoRows {
		return "", err
	}
	number, err := service.AllocateIssueNumber(ctx, qtx, wsID, service.ResolveIssueCountPolicy(ctx, h.Entitlements, wsID))
	if err != nil {
		return "", err
	}
	position, err := issueposition.NextTopPosition(ctx, tx, wsID, "todo")
	if err != nil {
		return "", err
	}
	issue, err := qtx.CreateIssueWithOrigin(ctx, db.CreateIssueWithOriginParams{
		ID: dbid.NewV7(), WorkspaceID: wsID, Title: incident.Title(c),
		Description: pgtype.Text{String: incident.Body(c, names), Valid: true}, Status: "todo", Priority: "urgent",
		CreatorType: "member", CreatorID: creator, Position: position, Number: number, ProjectID: project,
		CreatedVia: pgtype.Text{String: issuecreatedvia.Autopilot, Valid: true},
	})
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE issue SET metadata = metadata || $2::jsonb WHERE id = $1`, issue.ID, meta); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	resp := issueToResponse(issue, h.getIssuePrefix(context.WithoutCancel(ctx), wsID))
	h.publish(protocol.EventIssueCreated, util.UUIDToString(wsID), "member", util.UUIDToString(creator), map[string]any{"issue": resp})
	return util.UUIDToString(issue.ID), nil
}

// incidentComment writes one comment and moves the incident record in the same
// transaction. close=true also marks the incident closed and the issue done.
func (h *Handler) incidentComment(ctx context.Context, wsID, author pgtype.UUID, issueID, body string, runs []incident.Run, closeIt bool) error {
	id, err := util.ParseUUID(issueID)
	if err != nil {
		return err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT metadata FROM issue WHERE id = $1 FOR UPDATE`, id).Scan(&raw); err != nil {
		return err
	}
	var m struct {
		RunIDs []string `json:"incident_run_ids"`
	}
	_ = json.Unmarshal(raw, &m)
	patch := map[string]any{}
	if len(runs) > 0 {
		for _, r := range runs {
			m.RunIDs = append(m.RunIDs, r.ID)
		}
		patch["incident_run_ids"] = capIDs(m.RunIDs)
		patch["incident_last_failure_at"] = runs[len(runs)-1].FailedAt.UTC().Format(time.RFC3339Nano)
	}
	if closeIt {
		patch["incident_state"] = "closed"
	}
	pj, _ := json.Marshal(patch)
	status := ""
	if closeIt {
		status = "done"
	}
	if _, err := tx.Exec(ctx, `UPDATE issue SET metadata = metadata || $2::jsonb, status = CASE WHEN $3 <> '' THEN $3 ELSE status END,
		updated_at = now() WHERE id = $1`, id, pj, status); err != nil {
		return err
	}
	created, err := h.Queries.WithTx(tx).CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: id, WorkspaceID: wsID, AuthorType: "member", AuthorID: author,
		Content: body, Type: "comment", SuppressedAgentIds: []pgtype.UUID{},
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	issue, err := h.Queries.GetIssue(ctx, id)
	if err != nil {
		return nil // written; the live push is best effort
	}
	h.publish(protocol.EventCommentCreated, util.UUIDToString(wsID), "member", util.UUIDToString(author), map[string]any{
		"comment": commentToResponse(created.Comment(), nil, nil), "issue_title": issue.Title,
		"issue_status": issue.Status, "issue_revision": created.IssueRevision,
	})
	return nil
}

// incidentNotify sends the one notice. The flag flips first, inside the
// transaction that writes the inbox item, so a retry or a second server cannot send twice.
func (h *Handler) incidentNotify(ctx context.Context, wsID, member pgtype.UUID, issueID string, class incident.Class, recent []incident.Run, names incident.Names, cfg incident.Config, now time.Time) error {
	id, err := util.ParseUUID(issueID)
	if err != nil {
		return err
	}
	var cluster *incident.Cluster
	for _, c := range incident.Detect(recent, now, cfg) {
		if c.Class == class {
			c := c
			cluster = &c
		}
	}
	title, body := incident.NoticeTitle(class), fmt.Sprintf("Incident issue opened for %s.", class)
	if cluster != nil {
		body = incident.NoticeBody(*cluster, names)
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE issue SET metadata = metadata || '{"incident_notified":true}'::jsonb
		WHERE id = $1 AND COALESCE(metadata->>'incident_notified','false') <> 'true'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // already sent
	}
	item, inserted, err := inbox.CreateIfEligible(ctx, h.Queries.WithTx(tx), db.CreateInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: wsID, RecipientType: "member", RecipientID: member, IssueID: id,
		Type: "incident", Severity: "action_required", Title: title, Body: pgtype.Text{String: body, Valid: true},
		ActorType: pgtype.Text{String: "system", Valid: true}, Details: []byte("{}"),
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if inserted {
		h.publish(protocol.EventInboxNew, util.UUIDToString(wsID), "system", "", map[string]any{"item": inboxToResponse(item)})
	}
	return nil
}

func capIDs(ids []string) []string {
	if len(ids) > incidentMaxRuns {
		return ids[len(ids)-incidentMaxRuns:]
	}
	return ids
}
