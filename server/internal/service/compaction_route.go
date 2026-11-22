package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2913: where a compaction task runs. With background_agents.compact set,
// /compact and auto-compact queue a task for that agent (purpose 'compaction').
// When it cannot take the job, the DM agent keeps it exactly as before and the
// task context records why.

const (
	// taskPurposeCompaction is agent_task_queue.purpose of a dedicated
	// compaction task. Step 23 renames it to bg:compact.
	taskPurposeCompaction = "compaction"
	// compactionContextType keeps these rows apart from output-only background
	// tasks ("background"), which have their own claim path.
	compactionContextType = "chat_compaction"

	CompactFallbackArchived       = "agent_archived"
	CompactFallbackIneligible     = "agent_ineligible"
	CompactFallbackNoInvokeRight  = "no_invoke_right"
	CompactFallbackRuntimeOffline = "runtime_offline"
)

// compactionRoute is the answer for one enqueue. A zero route is today's path.
type compactionRoute struct {
	agent  db.Agent // the compaction agent; valid only when dedicated
	reason string   // why the configured agent was skipped; "" when none is configured
	// ponytail: only the workspace chain's primary slot, as the claim-time
	// authority does. Personal scope and the fallback slot land with step 24.
	dedicated bool
}

// routeCompaction runs in the caller's transaction, under the DM lock.
func routeCompaction(ctx context.Context, q *db.Queries, session db.ChatSession) (compactionRoute, error) {
	workspace, err := q.GetWorkspace(ctx, session.WorkspaceID)
	if err != nil {
		return compactionRoute{}, fmt.Errorf("load workspace for compaction route: %w", err)
	}
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(workspace.Settings, &settings)
	cfg, err := bgagents.Parse(settings["background_agents"], bgagents.WorkspaceScope)
	if err != nil || cfg[bgagents.Compact].AgentID == "" {
		return compactionRoute{}, nil // nothing set: the DM agent compacts
	}
	agentID, err := util.ParseUUID(cfg[bgagents.Compact].AgentID)
	if err != nil {
		return compactionRoute{reason: CompactFallbackIneligible}, nil
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: session.WorkspaceID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return compactionRoute{reason: CompactFallbackIneligible}, nil
	case err != nil:
		return compactionRoute{}, fmt.Errorf("load compaction agent: %w", err)
	case agent.ArchivedAt.Valid:
		return compactionRoute{reason: CompactFallbackArchived}, nil
	case !bgagents.WorkspaceEligible(agent):
		return compactionRoute{reason: CompactFallbackIneligible}, nil
	}
	allowed, err := memberMayInvokeAgent(ctx, q, agent, session.CreatorID, session.WorkspaceID)
	if err != nil {
		return compactionRoute{}, fmt.Errorf("check compaction agent invoke right: %w", err)
	}
	if !allowed {
		return compactionRoute{reason: CompactFallbackNoInvokeRight}, nil
	}
	runtime, err := q.GetAgentRuntime(ctx, agent.RuntimeID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return compactionRoute{}, fmt.Errorf("load compaction runtime: %w", err)
	}
	if err != nil || runtime.Status != "online" {
		return compactionRoute{reason: CompactFallbackRuntimeOffline}, nil
	}
	return compactionRoute{agent: agent, dedicated: true}, nil
}

// applyTo points a CreateChatTask insert at the route. A dedicated task carries
// the compaction agent and its runtime, purpose, both session columns and the
// chain context in the same insert; the DM agent's MCP overlay stays behind. A
// fallback keeps the DM agent and records the reason in the task context.
func (r compactionRoute) applyTo(p *db.CreateChatTaskParams, session db.ChatSession) error {
	ctxJSON := map[string]any{"type": compactionContextType}
	switch {
	case r.dedicated:
		if !session.ID.Valid || !r.agent.RuntimeID.Valid {
			return errors.New("dedicated compaction task needs a chat session and a runtime")
		}
		p.AgentID = r.agent.ID
		p.RuntimeID = r.agent.RuntimeID
		p.RuntimeMcpOverlay = nil
		p.RuntimeConnectedApps = nil
		p.Purpose = pgtype.Text{String: taskPurposeCompaction, Valid: true}
		p.ChatSessionID = session.ID
		p.SourceChatSessionID = session.ID
		ctxJSON["chain_source"] = string(bgagents.WorkspaceSource)
		ctxJSON["member_id"] = util.UUIDToString(session.CreatorID)
		ctxJSON["slot"] = string(bgagents.Primary)
	case r.reason != "":
		ctxJSON["fallback_reason"] = r.reason
	default:
		return nil
	}
	raw, err := json.Marshal(ctxJSON)
	if err != nil {
		return err
	}
	p.Context = raw
	return nil
}

// compactFallbackReason reads the reason back from a compaction task context.
func compactFallbackReason(task db.AgentTaskQueue) string {
	var c struct {
		Reason string `json:"fallback_reason"`
	}
	_ = json.Unmarshal(task.Context, &c)
	return c.Reason
}
