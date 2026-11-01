package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/agentpreset"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const presetLabelPrefix = "preset:"

// resolveClaimPreset picks the preset for one claim. A load error is logged
// and resolves to base: the run never fails because of a preset.
func (h *Handler) resolveClaimPreset(ctx context.Context, task *db.AgentTaskQueue, agent db.Agent, provider string) agentpreset.Result {
	in := agentpreset.Input{
		Agent: agentpreset.Agent{
			Provider:      provider,
			Model:         agent.Model.String,
			ThinkingLevel: agent.ThinkingLevel.String,
			ServiceTier:   agent.ServiceTier.String,
		},
		Task: agentpreset.Task{AgentID: uuidToString(agent.ID), SquadID: uuidToString(task.SquadID), IsLeaderTask: task.IsLeaderTask},
	}
	in.Labs = presetLabs()

	rows, err := h.Queries.ListAgentPresets(ctx, db.ListAgentPresetsParams{WorkspaceID: agent.WorkspaceID, AgentID: agent.ID})
	if err != nil {
		slog.Warn("daemon claim: could not load presets; using base", "task_id", uuidToString(task.ID), "error", err)
		return agentpreset.Resolve(in)
	}
	for _, row := range rows {
		in.Presets = append(in.Presets, presetFromRow(row))
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
		in.Step = h.workflowStepHandler(ctx, task)
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

// workflowStepHandler is the handler of the workflow step the task was queued
// for. Any lookup failure means no step preset: the run never fails because of
// one.
func (h *Handler) workflowStepHandler(ctx context.Context, task *db.AgentTaskQueue) *agentpreset.StepHandler {
	if !task.WorkflowStep.Valid || task.WorkflowStep.String == "" {
		return nil
	}
	issue, err := h.Queries.GetIssue(ctx, task.IssueID)
	if err != nil {
		slog.Warn("daemon claim: could not load issue for step preset", "task_id", uuidToString(task.ID), "error", err)
		return nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil {
		if err != nil {
			slog.Warn("daemon claim: could not load workflow for step preset", "task_id", uuidToString(task.ID), "error", err)
		}
		return nil
	}
	step, ok := def.Step(task.WorkflowStep.String)
	if !ok {
		return nil
	}
	return &agentpreset.StepHandler{Type: step.Handler.Type, ID: step.Handler.ID, Preset: step.Handler.Preset}
}
