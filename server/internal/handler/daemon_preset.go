package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/agentpreset"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const presetLabelPrefix = "preset:"

// resolveClaimPreset picks the preset for one claim. A load error is logged
// and resolves to base: the run never fails because of a preset. Workflow-step
// presets (level 3) are not wired yet, so Input.Step stays nil.
func (h *Handler) resolveClaimPreset(ctx context.Context, task *db.AgentTaskQueue, agent db.Agent, provider string) agentpreset.Result {
	in := agentpreset.Input{
		Agent: agentpreset.Agent{
			Provider:      provider,
			Model:         agent.Model.String,
			ThinkingLevel: agent.ThinkingLevel.String,
			ServiceTier:   agent.ServiceTier.String,
		},
		Task: agentpreset.Task{AgentID: uuidToString(agent.ID)},
	}
	// A malformed value means no lab lock, same as voteLab.
	_ = json.Unmarshal([]byte(os.Getenv("MULTICA_VOTE_GATE_LABS")), &in.Labs)

	rows, err := h.Queries.ListAgentPresets(ctx, db.ListAgentPresetsParams{WorkspaceID: agent.WorkspaceID, AgentID: agent.ID})
	if err != nil {
		slog.Warn("daemon claim: could not load presets; using base", "task_id", uuidToString(task.ID), "error", err)
		return agentpreset.Resolve(in)
	}
	for _, row := range rows {
		p := agentpreset.Preset{Name: row.Name, IsDefault: row.IsDefault}
		if row.Model.Valid {
			p.Model = &row.Model.String
		}
		if row.ThinkingLevel.Valid {
			p.ThinkingLevel = &row.ThinkingLevel.String
		}
		if row.ServiceTier.Valid {
			p.ServiceTier = &row.ServiceTier.String
		}
		if len(row.ContextArgs) > 0 {
			if err := json.Unmarshal(row.ContextArgs, &p.ContextArgs); err != nil {
				slog.Warn("daemon claim: unreadable preset context_args", "preset", row.Name, "error", err)
			}
		}
		in.Presets = append(in.Presets, p)
	}

	if task.ChatSessionID.Valid {
		session, err := h.Queries.GetChatSession(ctx, task.ChatSessionID)
		if err != nil {
			slog.Warn("daemon claim: could not load chat session preset", "task_id", uuidToString(task.ID), "error", err)
		} else if session.PresetName.Valid {
			in.DM = &session.PresetName.String
		}
	}
	if task.IssueID.Valid {
		labels, err := h.Queries.ListLabelsByIssue(ctx, db.ListLabelsByIssueParams{IssueID: task.IssueID, WorkspaceID: agent.WorkspaceID})
		if err != nil {
			slog.Warn("daemon claim: could not load issue labels", "task_id", uuidToString(task.ID), "error", err)
		}
		for _, l := range labels {
			if name, ok := strings.CutPrefix(l.Name, presetLabelPrefix); ok {
				in.IssueLabels = append(in.IssueLabels, name)
			}
		}
	}
	return agentpreset.Resolve(in)
}

// recordClaimSnapshot stores the requested (preset name, source, skips) and
// claimed (effective settings) record for this claim generation. contextArgs
// are the allow-listed context args of the merged payload, inherited or from
// the preset; known-empty is `[]`. It reports whether the record was written;
// on false every column stays NULL and the claim still succeeds.
func (h *Handler) recordClaimSnapshot(ctx context.Context, task *db.AgentTaskQueue, res agentpreset.Result, contextArgs []string) bool {
	skips := make([]map[string]string, 0, len(res.Skips))
	for _, s := range res.Skips {
		skips = append(skips, map[string]string{"level": string(s.Level), "name": s.Name, "reason": s.Reason})
	}
	params := db.SetTaskClaimSnapshotParams{
		ID:                   task.ID,
		RuntimeID:            task.RuntimeID,
		DispatchedAt:         task.DispatchedAt,
		PresetName:           pgtype.Text{String: res.Name, Valid: true},
		PresetSource:         pgtype.Text{String: string(res.Source), Valid: true},
		ClaimedModel:         pgtype.Text{String: res.Settings.Model, Valid: true},
		ClaimedThinkingLevel: pgtype.Text{String: res.Settings.ThinkingLevel, Valid: true},
		ClaimedServiceTier:   pgtype.Text{String: res.Settings.ServiceTier, Valid: true},
	}
	var err error
	if params.PresetSkips, err = json.Marshal(skips); err == nil {
		params.ClaimedContextArgs, err = json.Marshal(contextArgs)
	}
	if err == nil {
		var rows int64
		if rows, err = h.Queries.SetTaskClaimSnapshot(ctx, params); err == nil && rows == 1 {
			return true
		}
	}
	// An unrecorded claimed_model never counts as a different lab in the vote gate.
	slog.Warn("daemon claim: could not record the claim snapshot", "task_id", uuidToString(task.ID), "error", err)
	return false
}
