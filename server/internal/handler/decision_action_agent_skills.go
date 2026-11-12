package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	agentSkillsAction = "agent.skills.set"
	agentSkillsField  = "agent.skills"
)

var (
	errAgentSkillsNoChange = decisionIntentRefusal{http.StatusBadRequest, "no_change: the agent already has exactly these skills, all enabled"}
	errSelfEnableRefused   = decisionIntentRefusal{http.StatusConflict, "self_enable_refused: an agent may not add or enable a skill on itself"}
	errSkillsProposerGone  = decisionIntentRefusal{http.StatusForbidden, "proposer_required: the proposing agent is archived or gone"}
)

func init() {
	registerConfigChange(agentSkillsAction, configChange{
		Fields:     []string{agentSkillsField},
		Authorize:  authorizeAgentSkills,
		Lock:       lockAgentInstructionsTarget,
		Read:       readAgentSkills,
		Apply:      applyAgentSkills,
		Publish:    publishAgentSkillsSet,
		QueueField: agentSkillsField,
		// A file of a skill being added can change after the request; the
		// proposer's claim was right when it was made.
		AfterMismatchIsDrift: true,
	})
}

// readAgentSkills reads the live assignment and the proposed one under the row
// and skill locks the caller holds (E4b). The no-op check lives in Authorize:
// this read also serves as the post-write readback.
func readAgentSkills(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) (configChangeRead, error) {
	agent, err := loadAgentInstructionsTarget(ctx, queries, workspaceID, params)
	if err != nil {
		return configChangeRead{}, err
	}
	if agent.ArchivedAt.Valid {
		return configChangeRead{}, decisionIntentRefusal{http.StatusConflict, "agent_archived: the target agent is archived"}
	}
	var wanted decisionaction.AgentSkillsSetParams
	if err := json.Unmarshal(params, &wanted); err != nil || wanted.SkillIDs == nil {
		return configChangeRead{}, decisionIntentRefusal{http.StatusBadRequest, "invalid_params: params.skill_ids is required"}
	}
	proposed := make([]pgtype.UUID, len(wanted.SkillIDs))
	for i, id := range wanted.SkillIDs {
		if proposed[i], err = parseUUIDStrict(id); err != nil {
			return configChangeRead{}, decisionIntentRefusal{http.StatusBadRequest, "invalid_params: params.skill_ids holds an invalid id"}
		}
	}
	if len(proposed) > 0 {
		if err := refuseQoderSkills(ctx, queries, agent, len(proposed)); err != nil {
			return configChangeRead{}, err
		}
	}
	snapshot, err := loadSkillSetSnapshot(ctx, queries, workspaceID, agent.ID, proposed)
	if err != nil {
		return configChangeRead{}, err
	}
	before, after := make([]string, 0, len(snapshot.Live)), make([]string, 0, len(proposed))
	for id, enabled := range snapshot.Live {
		before = append(before, skillLine(snapshot.Names, id, enabled))
	}
	for _, id := range proposed {
		after = append(after, skillLine(snapshot.Names, uuidToString(id), true))
	}
	// Sorted by name first, so the diff shows the same order a reader expects.
	sort.Strings(before)
	sort.Strings(after)
	return configChangeRead{
		Field: agentSkillsField, TargetID: uuidToString(agent.ID),
		Before: before, After: after, BeforeLines: before, AfterLines: after,
		LiveSHA256: snapshot.LiveSHA256, AfterSHA256: snapshot.AfterSHA256,
		HumanBlocksIntact: true,
	}, nil
}

// skillLine is "name (id) [enabled|disabled]".
func skillLine(names map[string]string, id string, enabled bool) string {
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	return fmt.Sprintf("%s (%s) [%s]", names[id], id, state)
}

// refuseQoderSkills is the Qoder check of the ordinary skills route.
func refuseQoderSkills(ctx context.Context, queries *db.Queries, agent db.Agent, skills int) error {
	runtime, err := queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{ID: agent.RuntimeID, WorkspaceID: agent.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if runtime.Provider != "qoder_cloud" {
		return nil
	}
	if err := (qoderOverrides{skills: skills}).validate(); err != nil {
		return decisionIntentRefusal{http.StatusBadRequest, "qoder_managed: " + err.Error()}
	}
	return nil
}

// authorizeAgentSkills is the proposer policy. A change that alters nothing is
// refused. An agent may not widen its own skills: its after set may hold only
// skills that are enabled on it now, so it can remove skills and nothing else.
func authorizeAgentSkills(ctx context.Context, queries *db.Queries, workspaceID, proposer pgtype.UUID, params json.RawMessage, read configChangeRead) error {
	agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: proposer, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && agent.ArchivedAt.Valid) {
		return errSkillsProposerGone
	}
	if err != nil {
		return err
	}
	if read.LiveSHA256 == read.AfterSHA256 {
		return errAgentSkillsNoChange
	}
	if uuidToString(proposer) != read.TargetID {
		return nil
	}
	var wanted decisionaction.AgentSkillsSetParams
	if err := json.Unmarshal(params, &wanted); err != nil {
		return decisionIntentRefusal{http.StatusBadRequest, "invalid_params: params must be an object"}
	}
	assigned, err := queries.ListAgentSkillAssignments(ctx, proposer)
	if err != nil {
		return err
	}
	enabled := make(map[string]bool, len(assigned))
	for _, row := range assigned {
		enabled[uuidToString(row.SkillID)] = row.Enabled
	}
	for _, id := range wanted.SkillIDs {
		if !enabled[id] {
			return errSelfEnableRefused
		}
	}
	return nil
}

// applyAgentSkills replaces the assignment; every written row is enabled, which
// is what the preview showed.
func applyAgentSkills(ctx context.Context, execution decisionActionExecution, read configChangeRead) error {
	agentID, err := parseUUIDStrict(read.TargetID)
	if err != nil {
		return err
	}
	var wanted decisionaction.AgentSkillsSetParams
	if err := json.Unmarshal(execution.Action.Params, &wanted); err != nil {
		return err
	}
	if err := execution.Queries.RemoveAllAgentSkills(ctx, agentID); err != nil {
		return err
	}
	for _, id := range wanted.SkillIDs {
		skillID, err := parseUUIDStrict(id)
		if err != nil {
			return err
		}
		if err := execution.Queries.AddAgentSkill(ctx, db.AddAgentSkillParams{AgentID: agentID, SkillID: skillID}); err != nil {
			return err
		}
	}
	return nil
}

// publishAgentSkillsSet sends the event the ordinary skills route sends.
func publishAgentSkillsSet(ctx context.Context, handler *Handler, action db.DecisionAction) error {
	id, ok := targetParamID(action.Params, "agent_id")
	if !ok {
		return nil
	}
	skills, err := handler.agentSkillResponses(ctx, id)
	if err != nil {
		return err
	}
	handler.publish(protocol.EventAgentStatus, uuidToString(action.WorkspaceID), "member", uuidToString(action.ApprovedByUserID),
		map[string]any{"agent_id": uuidToString(id), "skills": skills})
	return nil
}
