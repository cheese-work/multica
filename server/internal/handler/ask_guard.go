package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// askGuardHit runs the guard for one agent operation and reports whether the
// operation may proceed. It writes the response when it may not: 503 with
// Retry-After when the event could not be written, 422 with fix in block mode.
// The event is committed before the 422, so the caller rolls back its own
// transaction afterwards without losing it.
func (h *Handler) askGuardHit(w http.ResponseWriter, r *http.Request, ws pgtype.UUID, op askguard.Op, fix string) bool {
	return refuseOnAskGuard(w, r.Context(), askguard.Guard{Queries: h.Queries, Metrics: h.Metrics}, ws, op, fix)
}

func refuseOnAskGuard(w http.ResponseWriter, ctx context.Context, g askguard.Guard, ws pgtype.UUID, op askguard.Op, fix string) bool {
	v, err := g.Hit(ctx, ws, op)
	if err != nil {
		slog.Warn("ask guard hit failed", "error", err, "kind", op.Kind)
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "ask guard unavailable; retry the same request")
		return false
	}
	if v.Block {
		writeError(w, http.StatusUnprocessableEntity, "this operation asks a person to act without a decision request; run: "+fix)
		return false
	}
	return true
}

// validateAskGuardSetting checks the ask_guard_mode key of a workspace
// settings object. An absent key is valid; null is not.
func validateAskGuardSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{askguard.SettingKey, askguard.ReversibleSettingKey, askguard.OwnerScriptSettingKey, askguard.WaitingAudienceSettingKey, askguard.WaitingOnSettingKey} {
		v, present := config[key]
		if !present {
			continue
		}
		s, _ := v.(string)
		if _, err := askguard.ParseMode(s); err != nil {
			return fmt.Errorf("%s must be off, warn or block", key)
		}
	}
	return nil
}

// discoveryBriefWorkflow is the workflow of a Discovery plan child, the only issue
// that may send the brief card (Plan step 13, CHE-6107 A5).
const discoveryBriefWorkflow = "Plan (gstack)"

const reversibleAskFix = "Standing Delivery Authorization (CHE-584) lets agents decide and execute reversible work without asking: do it and record it. Ask a person only for spend, deploy, publish, irreversible or product_direction, or use an approval-gated action request"

// refuseReversibleAsk (CHE-5754) refuses a plain agent question whose class is
// reversible: the ask is for work Standing Delivery Authorization already
// allows. Action, proposal and batch requests are the approval-gated path
// (CHE-5167) and stay open, as does the typed discovery_brief subject on an issue
// that follows the Plan (gstack) workflow (CHE-10271). The reversible_ask_guard setting decides: off allows,
// warn logs, block refuses. Set it to off to revert; no data changes.
func (handler *Handler) refuseReversibleAsk(ctx context.Context, issue db.Issue, input decisionRequestInput) error {
	if input.EscalationClass != "reversible" || input.Action != nil || input.Proposal != nil || input.Batch != nil || input.Kind == decisiontx.KindAction {
		return nil
	}
	if input.Parsed != nil && input.Parsed.Subject == decisiontx.SubjectDiscoveryBrief {
		// The subject is a caller-supplied label. It exempts the ask only on an
		// issue the server can verify follows the Discovery plan workflow.
		def, err := handler.issueWorkflowDef(ctx, issue)
		if err != nil {
			return err
		}
		if def != nil && def.Name == discoveryBriefWorkflow {
			return nil
		}
	}
	workspace, err := handler.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return err
	}
	mode, err := askguard.ReversibleModeFromSettings(workspace.Settings)
	if err != nil {
		return err
	}
	switch mode {
	case askguard.ModeBlock:
		return decisionIntentRefusal{http.StatusUnprocessableEntity, "this asks a person to approve reversible work; " + reversibleAskFix}
	case askguard.ModeWarn:
		slog.Warn("ask guard: reversible decision request", "issue_id", uuidToString(issue.ID))
	}
	return nil
}

const reversibleCommentFix = "Standing Delivery Authorization (CHE-584) lets agents decide and execute reversible work without asking: do it and record it. To ask a person for spend, deploy, publish, irreversible or product_direction run: multica decision request ...; add --notify-only to multica issue comment add if the comment needs no answer"

// askGuardReversibleComment (CHE-5754) is the issue-comment path of the
// reversible ask guard. A plain comment has no class, so no text is read. The
// structural facts are: an agent task, no typed kind (notify-only, decision
// intent, record and veto never reach here), an issue waiting on a person
// (blocked or need_decision), a parent comment written by a member, and no
// agent or squad mention. Such a reply is addressed to that member, which is
// the CHE-5281 shape. A top-level report, a reply under an agent and a handoff
// that mentions an agent or squad are never refused. block refuses with 422,
// warn logs, off allows. A run with no task or in a chat lane only warns: it
// has no run to recover through a refusal. Set the setting to off to revert;
// no data changes.
func (handler *Handler) askGuardReversibleComment(writer http.ResponseWriter, request *http.Request, issue db.Issue, task *db.AgentTaskQueue, parent *db.Comment, content string) bool {
	if (issue.Status != "blocked" && issue.Status != "need_decision") || parent == nil || parent.AuthorType != "member" || mentionsAgentOrSquad(content) {
		return true
	}
	workspace, err := handler.Queries.GetWorkspace(request.Context(), issue.WorkspaceID)
	var mode askguard.Mode
	if err == nil {
		mode, err = askguard.ReversibleModeFromSettings(workspace.Settings)
	}
	if err != nil {
		slog.Warn("reversible ask guard failed", "error", err, "issue_id", uuidToString(issue.ID))
		writer.Header().Set("Retry-After", "5")
		writeError(writer, http.StatusServiceUnavailable, "ask guard unavailable; retry the same request")
		return false
	}
	switch {
	case mode == askguard.ModeBlock && task != nil && !task.ChatSessionID.Valid:
		writeError(writer, http.StatusUnprocessableEntity, "this plain comment on a "+issue.Status+" issue waits on a person for work Standing Delivery Authorization may already allow; "+reversibleCommentFix)
		return false
	case mode != askguard.ModeOff:
		slog.Warn("ask guard: plain comment on a waiting issue", "issue_id", uuidToString(issue.ID), "status", issue.Status)
	}
	return true
}

func mentionsAgentOrSquad(content string) bool {
	for _, m := range util.ParseMentions(content) {
		if m.Type == "agent" || m.Type == "squad" {
			return true
		}
	}
	return false
}
