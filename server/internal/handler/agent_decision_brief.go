package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
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
	refusal error
	reason  string
}

func (e agentDecisionExcludedError) Error() string { return e.refusal.Error() }
func (e agentDecisionExcludedError) Unwrap() error { return e.refusal }

func agentDecisionExcluded(message, reason string) error {
	return agentDecisionExcludedError{refusal: agentDecisionRefusal(message), reason: reason}
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
	// WAIVED for a waiver gate; empty otherwise.
	Outcome string `json:"outcome,omitempty"`
	// Set when Jev decided for Cheese. Still an agent decision.
	Decider    string   `json:"decider,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
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
	member, ok := h.requireWorkspaceMember(w, r, uuidToString(workspaceID), "workspace not found")
	if !ok {
		return
	}
	askIDs := r.URL.Query()["jev_ask"]
	if len(askIDs) > agentDecisionBriefMaxJevAsks {
		writeError(w, http.StatusBadRequest, "too many jev_ask ids")
		return
	}
	for _, id := range askIDs {
		if _, ok := parseUUIDOrBadRequest(w, id, "jev_ask"); !ok {
			return
		}
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
        s.effective_at,s.revoked_at,s.revoke_reason,sc.scope->'Proposal'->>'decider',(sc.scope->'Proposal'->>'confidence')::float8,
        (SELECT v.comment_id::text FROM governed_vote_record v WHERE v.workspace_id=s.workspace_id AND v.decision_instance_id=s.instance_id AND v.decision_event='proposal' LIMIT 1)
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
		var reason, proposal, decider *string
		if err := rows.Scan(&rec.InstanceID, &rec.IssueID, &rec.Identifier, &rec.Gate, &sha, &effective, &revoked, &reason, &decider, &rec.Confidence, &proposal); err != nil {
			rows.Close()
			fail(err)
			return
		}
		rec.State = "effective"
		rec.Outcome = agentDecisionOutcome(rec.Gate)
		if decider != nil {
			rec.Decider = *decider
		}
		rec.EffectiveAt = effective.UTC().Format(time.RFC3339)
		rec.ReviewTargetAt = effective.Add(agentDecisionReviewTarget).UTC().Format(time.RFC3339)
		if revoked != nil {
			rec.State = "revoked"
			rec.RevokedAt = revoked.UTC().Format(time.RFC3339)
			if reason != nil {
				rec.RevokeReason = *reason
			}
		}
		rec.Link = h.agentDecisionLink(slug, rec.Identifier, proposal)
		rec.VetoLine = agentDecisionVetoLine(rec.InstanceID, sha, uuidToString(member.UserID))
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
	waivers, err := h.agentDecisionWaivers(ctx, workspaceID)
	if err != nil {
		fail(err)
		return
	}
	jev, err := h.agentDecisionJevMetrics(ctx, workspaceID)
	if err != nil {
		fail(err)
		return
	}
	jevAsks, err := h.agentDecisionJevAsks(ctx, workspaceID, askIDs)
	if err != nil {
		fail(err)
		return
	}
	// The run-budget auto-approver's audit line for the last day; "" when it did nothing.
	runBudgetAuto, err := h.runBudgetAutoBrief(ctx, workspaceID, time.Now().Add(-24*time.Hour))
	if err != nil {
		fail(err)
		return
	}
	runGuard, err := h.weeklyRunGuardBrief(ctx, workspaceID, time.Now().Add(-24*time.Hour))
	if err != nil {
		fail(err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_budget_auto":     runBudgetAuto,
		"run_guard":           runGuard,
		"enabled":             parsed.Enabled,
		"stopped":             parsed.Stopped,
		"review_target_hours": int(agentDecisionReviewTarget / time.Hour),
		"decisions":           records,
		"notice_failures":     failures,
		"pilot":               pilot,
		"waivers":             waivers,
		"jev":                 jev,
		"jev_asks":            jevAsks,
	})
}

// Most ids the CLI may ask about in one call.
const agentDecisionBriefMaxJevAsks = 1000

type agentDecisionWaiverCount struct {
	AgentID string `json:"agent_id"`
	Agent   string `json:"agent"`
	Count   int    `json:"count"`
}

// Waivers per agent: live test_first_red settlements, counted for the agent whose
// work was waived. A WAIVED record is never a PASS, so it is counted apart.
func (h *Handler) agentDecisionWaivers(ctx context.Context, workspaceID pgtype.UUID) ([]agentDecisionWaiverCount, error) {
	rows, err := h.DB.Query(ctx, `SELECT sc.proposer_agent_id::text,coalesce(a.name,''),count(*)
        FROM decision_agent_settlement s
        JOIN decision_agent_scope sc ON sc.instance_id=s.instance_id AND sc.workspace_id=s.workspace_id
        LEFT JOIN agent a ON a.id=sc.proposer_agent_id AND a.workspace_id=s.workspace_id
        WHERE s.workspace_id=$1 AND s.revoked_at IS NULL AND sc.waived_gate=$2
        GROUP BY 1,2 ORDER BY 3 DESC,2`, workspaceID, agentDecisionGateTestFirstRed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []agentDecisionWaiverCount{}
	for rows.Next() {
		var c agentDecisionWaiverCount
		if err := rows.Scan(&c.AgentID, &c.Agent, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type agentDecisionJevMetrics struct {
	Decisions int     `json:"decisions"`
	Vetoed    int     `json:"vetoed"`
	VetoRate  float64 `json:"veto_rate"`
	// Must stay zero: an excluded class is refused at propose time.
	ExcludedClassDecisions int `json:"excluded_class_decisions"`
}

func (h *Handler) agentDecisionJevMetrics(ctx context.Context, workspaceID pgtype.UUID) (agentDecisionJevMetrics, error) {
	var m agentDecisionJevMetrics
	err := h.DB.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE s.revoked_at IS NOT NULL),
        count(*) FILTER (WHERE sc.scope->'Proposal'->>'decision_class' = ANY($3))
        FROM decision_agent_settlement s
        JOIN decision_agent_scope sc ON sc.instance_id=s.instance_id AND sc.workspace_id=s.workspace_id
        WHERE s.workspace_id=$1 AND sc.scope->'Proposal'->>'decider'=$2`,
		workspaceID, agentDecisionDeciderJev, agentDecisionExcludedClasses).
		Scan(&m.Decisions, &m.Vetoed, &m.ExcludedClassDecisions)
	if err != nil {
		return m, err
	}
	if m.Decisions > 0 {
		m.VetoRate = float64(m.Vetoed) / float64(m.Decisions)
	}
	return m, nil
}

type agentDecisionJevAsk struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	// The option label Cheese chose; empty while the ask is open.
	Answer string `json:"answer"`
}

// For each ask id from the Jev shadow log: when it was asked and what Cheese chose.
func (h *Handler) agentDecisionJevAsks(ctx context.Context, workspaceID pgtype.UUID, ids []string) ([]agentDecisionJevAsk, error) {
	out := []agentDecisionJevAsk{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := h.DB.Query(ctx, `SELECT r.id::text,r.created_at,coalesce(r.ask_json->'options'->(a.chosen_option_index)::int->>'label','')
        FROM decision_request r
        LEFT JOIN decision_answer a ON a.decision_request_id=r.id AND a.disclaimed_at IS NULL
        WHERE r.workspace_id=$1 AND r.id = ANY($2::uuid[]) ORDER BY r.created_at`, workspaceID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a agentDecisionJevAsk
		var created time.Time
		if err := rows.Scan(&a.ID, &created, &a.Answer); err != nil {
			return nil, err
		}
		a.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, a)
	}
	return out, rows.Err()
}

// The line names the reader as undo owner, so pasting it needs no edit.
func agentDecisionVetoLine(instance, candidateSHA, memberUserID string) string {
	return "Veto: decision:" + instance + " " + candidateSHA + " member:" + memberUserID
}

// The link opens this instance's frozen proposal comment, not only the issue.
// It is root-relative: the web UI navigates in-app on whichever origin the
// reader used, so no deployment origin is baked into stored content.
func (h *Handler) agentDecisionLink(slug, identifier string, proposalCommentID *string) string {
	if proposalCommentID != nil {
		return weblink.Comment(slug, identifier, *proposalCommentID)
	}
	return weblink.Issue(slug, identifier)
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
		items, messages, err := h.sendAgentDecisionNotice(ctx, p.workspace, p.issue, p.instance, p.sha, p.gate)
		if err == nil {
			workspace := uuidToString(p.workspace)
			for _, item := range items {
				h.publish(protocol.EventInboxNew, workspace, "system", "", map[string]any{"item": inboxToResponse(item)})
			}
			for _, message := range messages {
				session, err := h.Queries.GetChatSession(ctx, message.ChatSessionID)
				if err != nil {
					failures = errors.Join(failures, err)
					continue
				}
				h.publishChatFor(protocol.EventChatMessage, workspace, "system", "", uuidToString(session.CreatorID), uuidToString(message.ChatSessionID), protocol.ChatMessagePayload{ChatSessionID: uuidToString(message.ChatSessionID), MessageID: uuidToString(message.ID), Role: message.Role, Content: message.Content, CreatedAt: timestampToString(message.CreatedAt), MessageKind: message.MessageKind})
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

// One transaction: an owner item per owner plus, where the owner has a usable
// topic thread (CHE-2407), a copy there. The notice counts as delivered only
// when every owner item was inserted; otherwise it rolls back and retries.
func (h *Handler) sendAgentDecisionNotice(ctx context.Context, workspaceID, issueID pgtype.UUID, instance, sha, gate string) ([]db.InboxItem, []db.ChatMessage, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	var slug, identifier, title string
	var proposal *string
	var proposer pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT w.slug,w.issue_prefix || '-' || i.number,i.title,sc.proposer_agent_id,
        (SELECT v.comment_id::text FROM governed_vote_record v WHERE v.workspace_id=sc.workspace_id AND v.decision_instance_id=sc.instance_id AND v.decision_event='proposal' LIMIT 1)
        FROM decision_agent_scope sc JOIN issue i ON i.id=$1 AND i.workspace_id=sc.workspace_id JOIN workspace w ON w.id=sc.workspace_id
        WHERE sc.instance_id=$3 AND sc.workspace_id=$2`, issueID, workspaceID, instance).Scan(&slug, &identifier, &title, &proposer, &proposal); err != nil {
		return nil, nil, fmt.Errorf("issue for decision notice: %w", err)
	}
	orows, err := tx.Query(ctx, `SELECT user_id FROM member WHERE workspace_id=$1 AND role='owner'`, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	var owners []pgtype.UUID
	for orows.Next() {
		var id pgtype.UUID
		if err := orows.Scan(&id); err != nil {
			orows.Close()
			return nil, nil, err
		}
		owners = append(owners, id)
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		return nil, nil, err
	}
	if len(owners) == 0 {
		return nil, nil, errors.New("no workspace owner to notify")
	}
	// The instance id marks this as a decision item, so it ignores the
	// unassigned-activity preference like any other decision.
	details, err := json.Marshal(map[string]string{"decision_instance": instance, "decision_request_id": instance})
	if err != nil {
		return nil, nil, err
	}
	link := h.agentDecisionLink(slug, identifier, proposal)
	queries := h.Queries.WithTx(tx)
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: issueID})
	if err != nil {
		return nil, nil, err
	}
	var items []db.InboxItem
	var messages []db.ChatMessage
	for _, owner := range owners {
		vetoLine := agentDecisionVetoLine(instance, sha, uuidToString(owner))
		body := agentDecisionNoticeBody(gate, identifier, title, agentDecisionRecordRef(link, false), vetoLine)
		chatBody := agentDecisionNoticeBody(gate, identifier, title, agentDecisionRecordRef(link, true), vetoLine)
		item, inserted, err := inbox.CreateIfEligible(ctx, queries, db.CreateInboxItemParams{
			ID: dbid.NewV7(), WorkspaceID: workspaceID, RecipientType: "member", RecipientID: owner,
			Type: "agent_decision_notice", Severity: "info", IssueID: issueID,
			Title: "Agents decided — veto?", Body: pgtype.Text{String: body, Valid: true},
			ActorType: pgtype.Text{String: "system", Valid: true}, Details: details,
		})
		if err != nil {
			return nil, nil, err
		}
		if !inserted {
			return nil, nil, fmt.Errorf("owner %s did not receive the notice item", uuidToString(owner))
		}
		items = append(items, item)
		message, err := h.copyNoticeToTopicThread(ctx, queries, issue, proposer, owner, chatBody)
		if err != nil {
			return nil, nil, err
		}
		if message.ID.Valid {
			messages = append(messages, message)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE decision_agent_notice SET delivered_at=clock_timestamp(),last_error='' WHERE instance_id=$1 AND workspace_id=$2 AND delivered_at IS NULL`, instance, workspaceID); err != nil {
		return nil, nil, err
	}
	return items, messages, tx.Commit(ctx)
}

// copyNoticeToTopicThread posts the notice in the owner's decision DM for the
// issue's topic, creating the DM when none exists. The inbox item is the
// reliable channel, so a thread the owner cannot use is skipped, not an error.
func (h *Handler) copyNoticeToTopicThread(ctx context.Context, queries *db.Queries, issue db.Issue, proposer, owner pgtype.UUID, body string) (db.ChatMessage, error) {
	asker, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: proposer})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ChatMessage{}, nil
	}
	if err != nil {
		return db.ChatMessage{}, err
	}
	topic, err := resolveDecisionTopic(ctx, queries, issue)
	if err != nil {
		return db.ChatMessage{}, err
	}
	steward, err := h.decisionSteward(ctx, queries, topic, asker, owner)
	if err != nil {
		return db.ChatMessage{}, err
	}
	if steward.ArchivedAt.Valid || !h.canInvokeAgent(ctx, steward, "member", uuidToString(owner), "", uuidToString(issue.WorkspaceID)) {
		return db.ChatMessage{}, nil
	}
	lane, found, err := topicLane(ctx, queries, issue, topic, owner, steward.ID)
	var refusal decisionIntentRefusal
	if errors.As(err, &refusal) {
		return db.ChatMessage{}, nil
	}
	if err != nil {
		return db.ChatMessage{}, err
	}
	if !found {
		lane, err = createDecisionDM(ctx, queries, issue, db.ChatSession{AgentID: steward.ID, CreatorID: owner, Title: "Decisions: " + topic.Title, DecisionTopicID: topic.ID})
		if err != nil {
			return db.ChatMessage{}, err
		}
	}
	message, err := queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: lane.ID, Role: "assistant", Content: body, MessageKind: pgtype.Text{String: "agent_decision", Valid: true}, SentVia: pgtype.Text{String: "system", Valid: true}})
	if err != nil {
		return db.ChatMessage{}, err
	}
	return message, queries.TouchChatSession(ctx, lane.ID)
}

const (
	agentDecisionJevKey    = "agent_decisions_jev"
	agentDecisionJevShadow = "shadow"
	agentDecisionJevLive   = "live"
)

// settings.agent_decisions_jev = {"agent_id", "mode"} names the one agent that
// may propose as Jev and whether its records only record (shadow) or settle
// (live). Absent means Jev delegation is off. It is independent of
// agent_decisions_enabled so the 7-day shadow holds with that switch on.
func agentDecisionJevSetting(ctx context.Context, q dbExecutor, workspaceID pgtype.UUID) (agentID, mode string, err error) {
	err = q.QueryRow(ctx, `SELECT coalesce(settings->'agent_decisions_jev'->>'agent_id',''),coalesce(settings->'agent_decisions_jev'->>'mode','')
        FROM workspace WHERE id=$1`, workspaceID).Scan(&agentID, &mode)
	return agentID, mode, err
}

// reconcileAgentDecisionJev makes a generic settings save keep the stored Jev
// registration: whatever the body says about the key is dropped. Only
// SetAgentDecisionJev (human owner) changes it.
func reconcileAgentDecisionJev(stored, incoming map[string]any) {
	delete(incoming, agentDecisionJevKey)
	if v, ok := stored[agentDecisionJevKey]; ok {
		incoming[agentDecisionJevKey] = v
	}
}

func requireAgentDecisionJev(ctx context.Context, q dbExecutor, workspaceID pgtype.UUID, proposer string) error {
	agentID, mode, err := agentDecisionJevSetting(ctx, q, workspaceID)
	if err != nil {
		return err
	}
	if agentID != proposer || (mode != agentDecisionJevShadow && mode != agentDecisionJevLive) {
		return agentDecisionRefusal("only the registered Jev agent may propose as Jev for Cheese, and only while Jev is in shadow or live mode")
	}
	return nil
}

// SetAgentDecisionJev registers the Jev agent and its mode (owner only).
// mode "off" clears the registration.
func (h *Handler) SetAgentDecisionJev(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceRole(w, r, uuidToString(workspaceID), "workspace not found", "owner"); !ok {
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
		Mode    string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.Mode == "off" {
		if _, err := h.DB.Exec(r.Context(), `UPDATE workspace SET settings=coalesce(settings,'{}'::jsonb) - 'agent_decisions_jev' WHERE id=$1`, workspaceID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save setting")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mode": "off"})
		return
	}
	agentUUID, ok := parseUUIDOrBadRequest(w, body.AgentID, "agent_id")
	if !ok {
		return
	}
	if body.Mode != agentDecisionJevShadow && body.Mode != agentDecisionJevLive {
		writeError(w, http.StatusBadRequest, "mode must be off, shadow or live")
		return
	}
	tag, err := h.DB.Exec(r.Context(), `UPDATE workspace w SET settings=jsonb_set(coalesce(w.settings,'{}'::jsonb),'{agent_decisions_jev}',jsonb_build_object('agent_id',$3::text,'mode',$4::text))
        WHERE w.id=$1 AND EXISTS (SELECT 1 FROM agent a WHERE a.id=$2 AND a.workspace_id=$1 AND a.archived_at IS NULL)`, workspaceID, agentUUID, uuidToString(agentUUID), body.Mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save setting")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusBadRequest, "agent not found in this workspace")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": uuidToString(agentUUID), "mode": body.Mode})
}

// The inbox shows its body as plain text; chat renders markdown, which links
// only a markdown link, not a bare root-relative path.
func agentDecisionRecordRef(link string, markdown bool) string {
	if markdown && link != "" {
		return "[open the record](" + link + ")"
	}
	return link
}

func agentDecisionNoticeBody(gate, identifier, title, recordRef, vetoLine string) string {
	return fmt.Sprintf("Agents decided %s on %s (%s).\nRecord: %s\nReview within %d hours; a later veto still works.\nTo veto, comment on the issue:\n%s\n\nfollowed by your reason.",
		gate, identifier, title, recordRef, int(agentDecisionReviewTarget/time.Hour), vetoLine)
}
