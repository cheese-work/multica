package issueworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// SettingsKeyQuickCreateDiscovery is the workspace settings key (CHE-9075,
// CHE-6107 C1) that lets quick-create file an unclear request in Discovery:
// "off" (default, also any other value) | "on".
const SettingsKeyQuickCreateDiscovery = "quick_create_discovery"

// DiscoveryStatusKey is the status of a workflow's Discovery step.
const DiscoveryStatusKey = "investigating"

// QuickCreateDiscovery reports whether the workspace turned the setting on.
// Only an explicit "on" does; an unset, unreadable or unknown value is off.
func QuickCreateDiscovery(settings []byte) bool {
	var parsed struct {
		Discovery string `json:"quick_create_discovery"`
	}
	_ = json.Unmarshal(settings, &parsed)
	return strings.ToLower(strings.TrimSpace(parsed.Discovery)) == "on"
}

// QuickCreateDiscoveryApplies reports whether a quick-create run may file an
// unclear request in Discovery: the setting is on and the workflow the new
// issue will follow lists the `investigating` step. projectID is the project
// the modal pinned (invalid when none). Without a project the issue takes the
// workspace Default. Every workflow the title-type map can route a title to
// must have the step too, because the agent picks the title.
func QuickCreateDiscoveryApplies(ctx context.Context, q Querier, settings []byte, workspaceID, projectID pgtype.UUID) (bool, error) {
	if !QuickCreateDiscovery(settings) {
		return false, nil
	}
	var def *Definition
	var err error
	if projectID.Valid {
		def, _, err = ForProject(ctx, q, workspaceID, projectID)
	} else {
		def, err = OutsideProject(ctx, q, workspaceID)
	}
	if err != nil || def == nil || !def.Has(DiscoveryStatusKey) {
		return false, err
	}
	for _, id := range workflowTypeIDs(settings) {
		def, _, err := ForIssue(ctx, q, workspaceID, id, projectID)
		if errors.Is(err, ErrOverrideMissing) {
			continue // the title-type reader ignores a dangling entry
		}
		if err != nil {
			return false, err
		}
		if def == nil || !def.Has(DiscoveryStatusKey) {
			return false, nil
		}
	}
	return true, nil
}

// workflowTypeIDs returns the valid workflow ids of workflow_by_type, sorted.
func workflowTypeIDs(settings []byte) []pgtype.UUID {
	var parsed struct {
		ByType map[string]string `json:"workflow_by_type"`
	}
	_ = json.Unmarshal(settings, &parsed)
	var ids []pgtype.UUID
	for _, raw := range parsed.ByType {
		var id pgtype.UUID
		if id.Scan(strings.TrimSpace(raw)) == nil && id.Valid {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return string(ids[i].Bytes[:]) < string(ids[j].Bytes[:]) })
	return ids
}
