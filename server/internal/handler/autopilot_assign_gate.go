package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5947 (retro W14): an issue to edit an autopilot went to an agent whose
// run could not write that autopilot, and sat blocked for 16 hours. The gate
// refuses that assignment at the API, where the autopilot write gate lives, so
// it holds for every client and runtime.
//
// An agent's run acts for the human at the top of its chain, so "can the
// assignee write the autopilot" is "can the human the RUN will act for write
// it": the same predicate as requireAutopilotWrite, judged on the originator the
// enqueue will stamp on the run, not on whoever makes the assignment. A member
// who assigns is that human. When an agent or the platform assigns, the run's
// human comes from the issue (its creator chain), not from the assigner's task.
// The issue names its target in its text, as an autopilot id next to the word
// "autopilot"; an id that is not an autopilot of this workspace is ignored.

const (
	autopilotAssignGateKey  = "autopilot_assign_gate"
	autopilotAssignNoWrite  = "autopilot_assign_no_write"
	maxAutopilotAssignIDs   = 3
	autopilotAssignModeOff  = "off"
	autopilotAssignModeWarn = "warn"
	// Default: block, a refusal costs one retry and a missed one costs hours.
	autopilotAssignModeBlock = "block"
)

var autopilotAssignUUIDRe = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// validateAutopilotAssignGateSetting checks the autopilot_assign_gate key of a
// workspace settings object. An absent key is valid; null is not.
func validateAutopilotAssignGateSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	v, present := config[autopilotAssignGateKey]
	if !present {
		return nil
	}
	if s, _ := v.(string); s == autopilotAssignModeOff || s == autopilotAssignModeWarn || s == autopilotAssignModeBlock {
		return nil
	}
	return fmt.Errorf("%s must be off, warn or block", autopilotAssignGateKey)
}

func autopilotAssignMode(settings []byte) string {
	var s map[string]any
	if len(settings) > 0 && json.Unmarshal(settings, &s) == nil {
		if m, _ := s[autopilotAssignGateKey].(string); m == autopilotAssignModeOff || m == autopilotAssignModeWarn {
			return m
		}
	}
	return autopilotAssignModeBlock
}

// refuseAutopilotAssign reports whether the assignment must stop; it has then
// written the 403.
func (h *Handler) refuseAutopilotAssign(w http.ResponseWriter, r *http.Request, workspaceID string, assigneeType pgtype.Text, assigneeID pgtype.UUID, title, description string, issue *db.Issue) bool {
	msg := h.autopilotAssignRefusal(r, workspaceID, assigneeType, assigneeID, title, description, issue)
	if msg == "" {
		return false
	}
	writeErrorCode(w, http.StatusForbidden, autopilotAssignNoWrite, msg)
	return true
}

// runHumanForAssignment is the human the run an assignment starts will act for,
// and whether the platform can name one. issue is the issue being reassigned, or
// nil on create. A member acts for themselves. An agent that creates an issue
// passes its own originator on (the issue's agent_create origin). An agent or the
// platform reassigning an existing issue starts a run attributed from the issue,
// exactly as the enqueue does; none then means the run has no human at all.
func (h *Handler) runHumanForAssignment(r *http.Request, workspaceID string, issue *db.Issue) (userID string, none bool) {
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	if actorType == "member" || issue == nil {
		return h.invokeOriginatorFromRequest(r, actorType, actorID), false
	}
	if h.TaskService == nil {
		return "", false
	}
	userID = uuidToString(h.TaskService.OriginatorForIssueTask(r.Context(), *issue, pgtype.UUID{}))
	return userID, userID == ""
}

// autopilotAssignRefusal returns the refusal message, or "" when the assignment
// may go on. The check opens when it cannot judge (an unreadable setting, no
// human behind a create): the gate never blocks work it cannot read. A run that
// is known to have no human cannot write an autopilot, so that is refused.
func (h *Handler) autopilotAssignRefusal(r *http.Request, workspaceID string, assigneeType pgtype.Text, assigneeID pgtype.UUID, title, description string, issue *db.Issue) string {
	if !assigneeID.Valid || (assigneeType.String != "agent" && assigneeType.String != "squad") {
		return ""
	}
	text := title + "\n" + description
	if !strings.Contains(strings.ToLower(text), "autopilot") {
		return ""
	}
	ctx := r.Context()
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return ""
	}
	ws, err := h.Queries.GetWorkspace(ctx, wsUUID)
	if err != nil {
		return ""
	}
	mode := autopilotAssignMode(ws.Settings)
	if mode == autopilotAssignModeOff {
		return ""
	}
	userID, none := h.runHumanForAssignment(r, workspaceID, issue)
	var member db.Member
	if !none {
		if userID == "" {
			return ""
		}
		if member, err = h.getWorkspaceMember(ctx, userID, workspaceID); err != nil {
			return ""
		}
	}
	seen := map[string]bool{}
	for _, id := range autopilotAssignUUIDRe.FindAllString(text, -1) {
		id = strings.ToLower(id)
		if seen[id] {
			continue
		}
		if seen[id] = true; len(seen) > maxAutopilotAssignIDs {
			break
		}
		apUUID, err := util.ParseUUID(id)
		if err != nil {
			continue
		}
		ap, err := h.Queries.GetAutopilotInWorkspace(ctx, db.GetAutopilotInWorkspaceParams{ID: apUUID, WorkspaceID: wsUUID})
		if err != nil || (!none && h.memberCanWriteAutopilot(ctx, ap, member)) {
			continue
		}
		if mode == autopilotAssignModeWarn {
			slog.Warn("autopilot assign gate (warn): assignee cannot write the autopilot", "autopilot_id", id, "workspace_id", workspaceID)
			continue
		}
		return fmt.Sprintf(
			"autopilot %s has can_write=false for this run, so the assignee cannot edit it; assign the issue to a run that acts for "+
				"the autopilot's creator, a workspace admin or a granted collaborator, or have a collaborator grant added first", id)
	}
	return ""
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// textOr returns the new text when the update sets it, else the stored text.
func textOr(updated pgtype.Text, stored string) string {
	if updated.Valid {
		return updated.String
	}
	return stored
}

// assigneeChanged reports whether an update hands the issue to a different
// assignee than it has now; a write that keeps the assignee is not an assignment.
func assigneeChanged(prev db.Issue, params db.UpdateIssueParams) bool {
	return prev.AssigneeType.String != params.AssigneeType.String || prev.AssigneeID != params.AssigneeID
}
