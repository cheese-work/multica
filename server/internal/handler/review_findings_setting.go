package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// review_findings_wakeup is workspace-scoped. It sets what a reviewer's CHANGES
// handoff wakeup (Agent Review step 4b) must carry: mode warn (default) accepts a
// handoff without --finding and answers with a warning, so a workflow whose step
// text does not pass findings yet keeps working; mode block refuses it with the
// same error a status verdict gets; mode off neither warns nor refuses. Findings
// that are sent are always checked.
const reviewFindingsWakeupKey = "review_findings_wakeup"

func validateReviewFindingsWakeupSetting(raw any) error {
	settings, _ := raw.(map[string]any)
	value, ok := settings[reviewFindingsWakeupKey]
	if !ok || value == nil {
		return nil
	}
	patch, _ := value.(map[string]any)
	if patch == nil {
		return fmt.Errorf("%s must be an object with mode off, warn, or block", reviewFindingsWakeupKey)
	}
	for key, v := range patch {
		if key != "mode" {
			return fmt.Errorf("%s.%s is not settable; use mode", reviewFindingsWakeupKey, key)
		}
		if mode, _ := v.(string); mode != "off" && mode != "warn" && mode != "block" {
			return fmt.Errorf("%s.mode must be off, warn, or block", reviewFindingsWakeupKey)
		}
	}
	return nil
}

func reviewFindingsWakeupMode(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) string {
	workspace, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "warn"
	}
	var settings struct {
		Mode struct {
			Mode string `json:"mode"`
		} `json:"review_findings_wakeup"`
	}
	if json.Unmarshal(workspace.Settings, &settings) != nil {
		return "warn"
	}
	switch settings.Mode.Mode {
	case "off", "block":
		return settings.Mode.Mode
	}
	return "warn"
}
