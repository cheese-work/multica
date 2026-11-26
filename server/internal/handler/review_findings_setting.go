package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// review_findings_category is workspace-scoped. mode optional (default) accepts a
// finding without a category, so reviewers whose step text still sends
// `--finding "P1:<summary>"` keep working after the server deploys; a category
// that is sent is always checked. mode required refuses a finding without one
// (review_finding_category_required). Rollout: deploy server and CLI, update the
// reviewer step text to send `P1@<category>:<summary>`, then set mode required.
const reviewFindingsCategoryKey = "review_findings_category"

func validateReviewFindingsCategorySetting(raw any) error {
	settings, _ := raw.(map[string]any)
	value, ok := settings[reviewFindingsCategoryKey]
	if !ok || value == nil {
		return nil
	}
	patch, _ := value.(map[string]any)
	if patch == nil {
		return fmt.Errorf("%s must be an object with mode optional or required", reviewFindingsCategoryKey)
	}
	for key, v := range patch {
		if key != "mode" {
			return fmt.Errorf("%s.%s is not settable; use mode", reviewFindingsCategoryKey, key)
		}
		if mode, _ := v.(string); mode != "optional" && mode != "required" {
			return fmt.Errorf("%s.mode must be optional or required", reviewFindingsCategoryKey)
		}
	}
	return nil
}

// reviewFindingsCategoryRequired reports whether the workspace refuses findings
// without a category. Any read error leaves the category optional.
func reviewFindingsCategoryRequired(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) bool {
	workspace, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return false
	}
	var settings struct {
		Category struct {
			Mode string `json:"mode"`
		} `json:"review_findings_category"`
	}
	return json.Unmarshal(workspace.Settings, &settings) == nil && settings.Category.Mode == "required"
}
