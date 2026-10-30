package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/weblink"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The brief section "Agents decided — veto?" is fed by this endpoint. The
// daily brief itself is a Multica autopilot owned by agent x99-claude-opus; it
// reads this feed through `multica decision brief` and prints the section. The
// target is a review aim, never an expiry: a record stays vetoable after it.
const agentDecisionReviewTarget = 24 * time.Hour

const agentDecisionNoticeBatch = 50

// settings.agent_decisions_enabled gates every new effective record. Default
// off: a missing key is off. The stop rule and the owner setting both write it.
func agentDecisionsEnabled(ctx context.Context, q dbExecutor, workspaceID pgtype.UUID) (bool, error) {
	var on bool
	err := q.QueryRow(ctx, `SELECT coalesce(settings->>'agent_decisions_enabled','')='true' FROM workspace WHERE id=$1`, workspaceID).Scan(&on)
	return on, err
}

// One excluded-category decision turns the setting off. Existing records stay
// vetoable: veto never reads the setting. Only an enabled workspace records a
// stop, so the first reason is kept.
func stopAgentDecisions(ctx context.Context, q dbExecutor, workspaceID pgtype.UUID, reason string) error {
	_, err := q.Exec(ctx, `UPDATE workspace SET settings=jsonb_set(jsonb_set(coalesce(settings,'{}'::jsonb),'{agent_decisions_enabled}','false'),
        '{agent_decisions_stopped}',jsonb_build_object('reason',$2::text,'at',to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"')))
        WHERE id=$1 AND settings->>'agent_decisions_enabled'='true'`, workspaceID, reason)
	return err
}

// A proposal or stored record outside the allowlist is an excluded category.
type agentDecisionExcludedError struct {
	refusal decisionIntentRefusal
	reason  string
}

func (e agentDecisionExcludedError) Error() string { return e.refusal.Error() }
func (e agentDecisionExcludedError) Unwrap() error { return e.refusal }

func agentDecisionExcluded(message, reason string) error {
	return agentDecisionExcludedError{refusal: agentDecisionRefusal(message).(decisionIntentRefusal), reason: reason}
}

func (h *Handler) SetAgentDecisionSetting(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceRole(w, r, uuidToString(workspaceID), "workspace not found", "owner"); !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	// Turning it on clears an earlier stop; the owner has read it.
	if _, err := h.DB.Exec(r.Context(), `UPDATE workspace SET settings=jsonb_set(coalesce(settings,'{}'::jsonb),'{agent_decisions_enabled}',to_jsonb($2::boolean))
        - CASE WHEN $2::boolean THEN 'agent_decisions_stopped' ELSE '' END WHERE id=$1`, workspaceID, *body.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save setting")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": *body.Enabled})
}

type agentDecisionBriefRecord struct {
	InstanceID     string `json:"instance_id"`
	IssueID        string `json:"issue_id"`
	Identifier     string `json:"identifier"`
	Gate           string `json:"gate"`
	State          string `json:"state"`
	EffectiveAt    string `json:"effective_at"`
	ReviewTargetAt string `json:"review_target_at"`
	RevokedAt      string `json:"revoked_at,omitempty"`
	RevokeReason   string `json:"revoke_reason,omitempty"`
	Link           string `json:"link"`
	VetoLine       string `json:"veto_line"`
}

type agentDecisionNoticeFailure struct {
	InstanceID    string `json:"instance_id"`
	Attempts      int    `json:"attempts"`
	LastError     string `json:"last_error"`
	NextAttemptAt string `json:"next_attempt_at"`
}

func (h *Handler) GetAgentDecisionBrief(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceMember(w, r, uuidToString(workspaceID), "workspace not found"); !ok {
		return
	}
	ctx := r.Context()
	fail := func(err error) {
		slog.Warn("agent decision brief failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read agent decisions")
	}
	var settings []byte
	var slug string
	if err := h.DB.QueryRow(ctx, `SELECT coalesce(settings,'{}'::jsonb),slug FROM workspace WHERE id=$1`, workspaceID).Scan(&settings, &slug); err != nil {
		fail(err)
		return
	}
	var parsed struct {
		Enabled bool `json:"agent_decisions_enabled"`
		Stopped *struct {
			Reason string `json:"reason"`
			At     string `json:"at"`
		} `json:"agent_decisions_stopped"`
	}
	_ = json.Unmarshal(settings, &parsed)

	// Every live record, and revoked ones from the last week.
	rows, err := h.DB.Query(ctx, `SELECT s.instance_id::text,s.issue_id::text,w.issue_prefix || '-' || i.number,sc.waived_gate,sc.candidate_sha256,
        s.effective_at,s.revoked_at,s.revoke_reason
        FROM decision_agent_settlement s
        JOIN decision_agent_scope sc ON sc.instance_id=s.instance_id AND sc.workspace_id=s.workspace_id
        JOIN issue i ON i.id=s.issue_id AND i.workspace_id=s.workspace_id
        JOIN workspace w ON w.id=s.workspace_id
        WHERE s.workspace_id=$1 AND (s.revoked_at IS NULL OR s.revoked_at > clock_timestamp() - interval '7 days')
        ORDER BY s.effective_at DESC LIMIT 100`, workspaceID)
	if err != nil {
		fail(err)
		return
	}
	records := []agentDecisionBriefRecord{}
	for rows.Next() {
		var rec agentDecisionBriefRecord
		var sha string
		var effective time.Time
		var revoked *time.Time
		var reason *string
		if err := rows.Scan(&rec.InstanceID, &rec.IssueID, &rec.Identifier, &rec.Gate, &sha, &effective, &revoked, &reason); err != nil {
			rows.Close()
			fail(err)
			return
		}
		rec.State = "effective"
		rec.EffectiveAt = effective.UTC().Format(time.RFC3339)
		rec.ReviewTargetAt = effective.Add(agentDecisionReviewTarget).UTC().Format(time.RFC3339)
		if revoked != nil {
			rec.State = "revoked"
			rec.RevokedAt = revoked.UTC().Format(time.RFC3339)
			if reason != nil {
				rec.RevokeReason = *reason
			}
		}
		rec.Link = h.agentDecisionLink(slug, rec.Identifier)
		rec.VetoLine = agentDecisionVetoLine(rec.InstanceID, sha)
		records = append(records, rec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fail(err)
		return
	}

	failures := []agentDecisionNoticeFailure{}
	frows, err := h.DB.Query(ctx, `SELECT instance_id::text,attempts,last_error,next_attempt_at FROM decision_agent_notice
        WHERE workspace_id=$1 AND delivered_at IS NULL AND attempts>0 ORDER BY created_at LIMIT 100`, workspaceID)
	if err != nil {
		fail(err)
		return
	}
	for frows.Next() {
		var f agentDecisionNoticeFailure
		var next time.Time
		if err := frows.Scan(&f.InstanceID, &f.Attempts, &f.LastError, &next); err != nil {
			frows.Close()
			fail(err)
			return
		}
		f.NextAttemptAt = next.UTC().Format(time.RFC3339)
		failures = append(failures, f)
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		fail(err)
		return
	}

	var pilot struct {
		Effective   int   `json:"effective"`
		Vetoed      int   `json:"vetoed"`
		Denominator int   `json:"denominator"`
		AsksAvoided int   `json:"asks_avoided"`
		Runs        int   `json:"runs"`
		RunTokens   int64 `json:"run_tokens"`
	}
	// A settlement row is one decision made; a veto before effect leaves a
	// revoked row, so it counts as a veto against the same denominator. One
	// ask is avoided once per issue, gate and frozen candidate.
	if err := h.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE s.revoked_at IS NULL),count(*) FILTER (WHERE s.revoked_at IS NOT NULL),count(*),
        count(DISTINCT (s.issue_id,sc.waived_gate,sc.candidate_sha256)) FILTER (WHERE s.revoked_at IS NULL)
        FROM decision_agent_settlement s JOIN decision_agent_scope sc ON sc.instance_id=s.instance_id AND sc.workspace_id=s.workspace_id
        WHERE s.workspace_id=$1`, workspaceID).Scan(&pilot.Effective, &pilot.Vetoed, &pilot.Denominator, &pilot.AsksAvoided); err != nil {
		fail(err)
		return
	}
	// Cost covers every run that wrote a protocol event (commit, reveal, proposal,
	// veto), so commit-reveal runs are inside the figure.
	if err := h.DB.QueryRow(ctx, `SELECT count(*),coalesce(sum(u.input_tokens+u.output_tokens),0)::bigint
        FROM (SELECT DISTINCT source_task_id FROM governed_vote_record WHERE workspace_id=$1 AND decision_instance_id IS NOT NULL AND source_task_id IS NOT NULL) v
        LEFT JOIN task_usage u ON u.task_id=v.source_task_id`, workspaceID).Scan(&pilot.Runs, &pilot.RunTokens); err != nil {
		fail(err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":             parsed.Enabled,
		"stopped":             parsed.Stopped,
		"review_target_hours": int(agentDecisionReviewTarget / time.Hour),
		"decisions":           records,
		"notice_failures":     failures,
		"pilot":               pilot,
	})
}

func agentDecisionVetoLine(instance, candidateSHA string) string {
	return "Veto: decision:" + instance + " " + candidateSHA
}

func (h *Handler) agentDecisionLink(slug, identifier string) string {
	path := weblink.Issue(slug, identifier)
	if link := weblink.Build(h.cfg.AppURL, path); link != "" {
		return link
	}
	return path
}

// Runs after commit. Each notice is its own transaction; a failure is stored on
// the row with exponential backoff and shows up in the brief.
func (h *Handler) deliverAgentDecisionNotices(ctx context.Context) error {
	rows, err := h.DB.Query(ctx, `SELECT n.instance_id::text,n.workspace_id,n.issue_id,sc.candidate_sha256,sc.waived_gate
        FROM decision_agent_notice n JOIN decision_agent_scope sc ON sc.instance_id=n.instance_id AND sc.workspace_id=n.workspace_id
        WHERE n.delivered_at IS NULL AND n.next_attempt_at<=clock_timestamp() ORDER BY n.created_at LIMIT $1`, agentDecisionNoticeBatch)
	if err != nil {
		return err
	}
	type pending struct {
		instance, sha, gate string
		workspace, issue    pgtype.UUID
	}
	var due []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.instance, &p.workspace, &p.issue, &p.sha, &p.gate); err != nil {
			rows.Close()
			return err
		}
		due = append(due, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var failures error
	for _, p := range due {
		items, err := h.sendAgentDecisionNotice(ctx, p.workspace, p.issue, p.instance, p.sha, p.gate)
		if err == nil {
			for _, item := range items {
				h.publish(protocol.EventInboxNew, uuidToString(p.workspace), "system", "", map[string]any{"item": inboxToResponse(item)})
			}
			continue
		}
		if _, recordErr := h.DB.Exec(ctx, `UPDATE decision_agent_notice SET attempts=attempts+1,last_error=left($3,500),
            next_attempt_at=clock_timestamp()+make_interval(secs => least(3600, 30 * power(2, least(attempts,10))))
            WHERE instance_id=$1 AND workspace_id=$2 AND delivered_at IS NULL`, p.instance, p.workspace, err.Error()); recordErr != nil {
			failures = errors.Join(failures, recordErr)
		}
	}
	return failures
}

func (h *Handler) sendAgentDecisionNotice(ctx context.Context, workspaceID, issueID pgtype.UUID, instance, sha, gate string) ([]db.InboxItem, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var slug, identifier, title string
	if err := tx.QueryRow(ctx, `SELECT w.slug,w.issue_prefix || '-' || i.number,i.title FROM issue i JOIN workspace w ON w.id=i.workspace_id WHERE i.id=$1 AND i.workspace_id=$2`, issueID, workspaceID).Scan(&slug, &identifier, &title); err != nil {
		return nil, fmt.Errorf("issue for decision notice: %w", err)
	}
	orows, err := tx.Query(ctx, `SELECT user_id FROM member WHERE workspace_id=$1 AND role='owner'`, workspaceID)
	if err != nil {
		return nil, err
	}
	var owners []pgtype.UUID
	for orows.Next() {
		var id pgtype.UUID
		if err := orows.Scan(&id); err != nil {
			orows.Close()
			return nil, err
		}
		owners = append(owners, id)
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		return nil, err
	}
	if len(owners) == 0 {
		return nil, errors.New("no workspace owner to notify")
	}
	body := fmt.Sprintf("Agents decided %s on %s (%s).\nRecord: %s\nReview within %d hours; a later veto still works.\nTo veto, comment on the issue:\n%s\n\nfollowed by your reason.",
		gate, identifier, title, h.agentDecisionLink(slug, identifier), int(agentDecisionReviewTarget/time.Hour), agentDecisionVetoLine(instance, sha))
	details, err := json.Marshal(map[string]string{"decision_instance": instance})
	if err != nil {
		return nil, err
	}
	queries := h.Queries.WithTx(tx)
	var items []db.InboxItem
	for _, owner := range owners {
		item, inserted, err := inbox.CreateIfEligible(ctx, queries, db.CreateInboxItemParams{
			ID: dbid.NewV7(), WorkspaceID: workspaceID, RecipientType: "member", RecipientID: owner,
			Type: "agent_decision_notice", Severity: "info", IssueID: issueID,
			Title: "Agents decided — veto?", Body: pgtype.Text{String: body, Valid: true},
			ActorType: pgtype.Text{String: "system", Valid: true}, Details: details,
		})
		if err != nil {
			return nil, err
		}
		if inserted {
			items = append(items, item)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE decision_agent_notice SET delivered_at=clock_timestamp(),last_error='' WHERE instance_id=$1 AND workspace_id=$2 AND delivered_at IS NULL`, instance, workspaceID); err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}
