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
// assignee write the autopilot" is "can the human this request acts for write
// it": the same acting human and the same predicate as requireAutopilotWrite.
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
// written the 403. The check opens when it cannot judge (no human behind the
// run, an unreadable setting): the gate never blocks work it cannot read.
func (h *Handler) refuseAutopilotAssign(w http.ResponseWriter, r *http.Request, workspaceID string, assigneeType pgtype.Text, assigneeID pgtype.UUID, title, description string) bool {
	if !assigneeID.Valid || (assigneeType.String != "agent" && assigneeType.String != "squad") {
		return false
	}
	text := title + "\n" + description
	if !strings.Contains(strings.ToLower(text), "autopilot") {
		return false
	}
	ctx := r.Context()
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return false
	}
	ws, err := h.Queries.GetWorkspace(ctx, wsUUID)
	if err != nil {
		return false
	}
	mode := autopilotAssignMode(ws.Settings)
	if mode == autopilotAssignModeOff {
		return false
	}
	userID := h.autopilotActingUserID(r, workspaceID)
	if userID == "" {
		return false
	}
	member, err := h.getWorkspaceMember(ctx, userID, workspaceID)
	if err != nil {
		return false
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
		if err != nil || h.memberCanWriteAutopilot(ctx, ap, member) {
			continue
		}
		if mode == autopilotAssignModeWarn {
			slog.Warn("autopilot assign gate (warn): assignee cannot write the autopilot", "autopilot_id", id, "workspace_id", workspaceID)
			continue
		}
		writeErrorCode(w, http.StatusForbidden, autopilotAssignNoWrite, fmt.Sprintf(
			"autopilot %s has can_write=false for this run, so the assignee cannot edit it; assign the issue to a run that acts for "+
				"the autopilot's creator, a workspace admin or a granted collaborator, or have a collaborator grant added first", id))
		return true
	}
	return false
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
