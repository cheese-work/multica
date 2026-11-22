package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/capabilityrouting"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// capability_routing (CHE-7666) is a governed setting: it changes only through
// the workspace.settings.patch card, so a generic settings save keeps the
// stored value, as for the routing agent.
const capabilityRoutingKey = capabilityrouting.SettingsKey

func reconcileCapabilityRouting(stored, incoming map[string]any) {
	delete(incoming, capabilityRoutingKey)
	if v, ok := stored[capabilityRoutingKey]; ok {
		incoming[capabilityRoutingKey] = v
	}
}

// checkCapabilityRoutingAgents refuses a patch whose capability_routing names
// an id that is not an agent of the workspace. Existence is the check, not
// reachability or archive state.
func checkCapabilityRoutingAgents(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, patch map[string]json.RawMessage) error {
	raw := patch[capabilityRoutingKey]
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	cfg, err := capabilityrouting.Parse(raw)
	if err != nil {
		return settingsInvalid("%v", err)
	}
	for _, id := range cfg.AgentIDs() {
		agentID, err := util.ParseUUID(id)
		if err == nil {
			_, err = q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: workspaceID})
		}
		if err != nil {
			return settingsInvalid("%s", fmt.Sprintf("%s names %s, which is not an agent of this workspace", capabilityRoutingKey, id))
		}
	}
	return nil
}
