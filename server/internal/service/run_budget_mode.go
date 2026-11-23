package service

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Run budget modes (workspace setting settings.run_budget.mode, CHE-9026).
// Block, today's behavior, is the default: a spent budget refuses derived runs
// and opens a card. Warn counts runs and reports the overage but never
// refuses. Off neither refuses, warns nor reports.
const (
	RunBudgetModeOff   = "off"
	RunBudgetModeWarn  = "warn"
	RunBudgetModeBlock = "block"
)

// RunBudgetModeFromSettings reads settings.run_budget.mode. Absent or
// unreadable, the budget blocks.
func RunBudgetModeFromSettings(settings []byte) string {
	var doc struct {
		Budget struct {
			Mode string `json:"mode"`
		} `json:"run_budget"`
	}
	if json.Unmarshal(settings, &doc) != nil {
		return RunBudgetModeBlock
	}
	switch doc.Budget.Mode {
	case RunBudgetModeOff, RunBudgetModeWarn:
		return doc.Budget.Mode
	}
	return RunBudgetModeBlock
}

// runBudgetMode is the mode of the workspace. A failed read blocks, so an
// unknown mode never lets a run through.
func runBudgetMode(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) string {
	ws, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return RunBudgetModeBlock
	}
	return RunBudgetModeFromSettings(ws.Settings)
}
