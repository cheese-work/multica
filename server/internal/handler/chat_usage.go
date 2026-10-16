package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// maxChatUsageChainHops bounds the continuation walk in each direction.
const maxChatUsageChainHops = 100

type ChatUsageTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

type ChatUsageRuns struct {
	ByStatus map[string]int `json:"by_status"`
	// Terminal runs split into Metered (a task_usage row exists) and Unreported (none).
	Terminal   int   `json:"terminal"`
	Metered    int   `json:"metered"`
	Unreported int   `json:"unreported"`
	TotalRunMs int64 `json:"total_run_ms"`
}

type ChatUsageModel struct {
	Tokens       ChatUsageTokens `json:"tokens"`
	CostUsdTicks int64           `json:"cost_usd_ticks"`
}

// ChatUsageChain is the "whole conversation" total across a continuation
// chain: the viewed DM plus every readable DM linked by continued_from /
// continued_into, /agent handoff hops included. Forks are not part of it.
type ChatUsageChain struct {
	SessionCount int `json:"session_count"`
	// Partial is true when the walk stopped at the hop limit or a session in
	// the chain was left out because the caller cannot read it.
	Partial        bool                      `json:"partial"`
	Tokens         ChatUsageTokens           `json:"tokens"`
	UncostedTokens ChatUsageTokens           `json:"uncosted_tokens"`
	CostUsdTicks   int64                     `json:"cost_usd_ticks"`
	ByModel        map[string]ChatUsageModel `json:"by_model"`
	Runs           struct {
		Terminal   int   `json:"terminal"`
		Metered    int   `json:"metered"`
		Unreported int   `json:"unreported"`
		TotalRunMs int64 `json:"total_run_ms"`
	} `json:"runs"`
}

type ChatSessionUsageResponse struct {
	SessionID      string          `json:"session_id"`
	Tokens         ChatUsageTokens `json:"tokens"`
	UncostedTokens ChatUsageTokens `json:"uncosted_tokens"`
	CostUsdTicks   int64           `json:"cost_usd_ticks"`
	Runs           ChatUsageRuns   `json:"runs"`
	Messages       struct {
		ByRole map[string]int `json:"by_role"`
	} `json:"messages"`
	ByModel                map[string]ChatUsageModel `json:"by_model"`
	ContinuedFromSessionID *string                   `json:"continued_from_session_id"`
	ContinuedIntoSessionID *string                   `json:"continued_into_session_id"`
	ForkedFromSessionID    *string                   `json:"forked_from_session_id"`
	// Chain is null for a DM with no readable continuation link, and for any
	// caller that is not an interactive user session.
	Chain *ChatUsageChain `json:"chain"`
}

// GetChatSessionUsage returns token, cost, run and message totals for one DM.
// It reads the session like GetChatSession, so creator, workspace,
// private-agent and interactive-credential refusals are identical.
func (h *Handler) GetChatSessionUsage(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	session, ok := h.chatReadSession(w, r, userID)
	if !ok {
		return
	}
	row, err := h.Queries.GetChatSessionUsageSummary(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get chat session usage")
		return
	}

	// by_status and by_role are open maps. The five common statuses and the two
	// message roles are always present; any other value (dispatched, deferred,
	// waiting_local_directory, ...) appears only when the session has it.
	byStatus := map[string]int{"completed": 0, "failed": 0, "cancelled": 0, "queued": 0, "running": 0}
	if err := json.Unmarshal(row.StatusCounts, &byStatus); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get chat session usage")
		return
	}
	byModel := map[string]ChatUsageModel{}
	byRole := map[string]int{"user": 0, "assistant": 0}
	if err := json.Unmarshal(row.ByModel, &byModel); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get chat session usage")
		return
	}
	if err := json.Unmarshal(row.RoleCounts, &byRole); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get chat session usage")
		return
	}

	resp := ChatSessionUsageResponse{
		SessionID: uuidToString(session.ID),
		Tokens: ChatUsageTokens{
			Input: row.InputTokens, Output: row.OutputTokens,
			CacheRead: row.CacheReadTokens, CacheWrite: row.CacheWriteTokens,
		},
		UncostedTokens: ChatUsageTokens{
			Input: row.UncostedInputTokens, Output: row.UncostedOutputTokens,
			CacheRead: row.UncostedCacheReadTokens, CacheWrite: row.UncostedCacheWriteTokens,
		},
		CostUsdTicks: row.CostUsdTicks,
		Runs: ChatUsageRuns{
			ByStatus: byStatus, Terminal: int(row.TerminalRuns),
			Metered: int(row.MeteredRuns), Unreported: int(row.UnreportedRuns), TotalRunMs: row.TotalRunMs,
		},
		ByModel:                byModel,
		ContinuedFromSessionID: uuidToPtr(session.ContinuedFromSessionID),
		ContinuedIntoSessionID: uuidToPtr(session.ContinuedIntoSessionID),
		ForkedFromSessionID:    uuidToPtr(session.ForkedFromSessionID),
	}
	resp.Messages.ByRole = byRole
	// A running chat task may read its own session, but the other sessions in
	// the chain are not its to see: only an interactive user gets the total.
	if r.Header.Get("X-Actor-Source") == "interactive_jwt" {
		chain, err := h.chatUsageChain(r, userID, session)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to get chat session usage")
			return
		}
		resp.Chain = chain
	}
	writeJSON(w, http.StatusOK, resp)
}

// chatUsageChain sums usage over the readable sessions of session's
// continuation chain. It returns nil when fewer than two are readable.
func (h *Handler) chatUsageChain(r *http.Request, userID string, session db.ChatSession) (*ChatUsageChain, error) {
	ctx := r.Context()
	members, err := h.Queries.ListChatSessionChain(ctx, db.ListChatSessionChainParams{
		SessionID: session.ID, WorkspaceID: session.WorkspaceID, CreatorID: session.CreatorID,
		MaxHops: maxChatUsageChainHops,
	})
	if err != nil {
		return nil, err
	}
	workspaceID := uuidToString(session.WorkspaceID)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	readable := map[pgtype.UUID]bool{}
	ids := make([]pgtype.UUID, 0, len(members))
	partial := false
	for _, m := range members {
		if m.Beyond {
			partial = true
			continue
		}
		ok, seen := readable[m.AgentID]
		if !seen {
			ok = h.canReadChainAgent(ctx, m.AgentID, actorType, actorID, workspaceID)
			readable[m.AgentID] = ok
		}
		if !ok {
			partial = true
			continue
		}
		ids = append(ids, m.ID)
	}
	if len(ids) < 2 {
		return nil, nil
	}
	row, err := h.Queries.GetChatSessionsUsageSummary(ctx, ids)
	if err != nil {
		return nil, err
	}
	byModel := map[string]ChatUsageModel{}
	if err := json.Unmarshal(row.ByModel, &byModel); err != nil {
		return nil, err
	}
	chain := &ChatUsageChain{
		SessionCount: len(ids), Partial: partial, ByModel: byModel, CostUsdTicks: row.CostUsdTicks,
		Tokens: ChatUsageTokens{
			Input: row.InputTokens, Output: row.OutputTokens,
			CacheRead: row.CacheReadTokens, CacheWrite: row.CacheWriteTokens,
		},
		UncostedTokens: ChatUsageTokens{
			Input: row.UncostedInputTokens, Output: row.UncostedOutputTokens,
			CacheRead: row.UncostedCacheReadTokens, CacheWrite: row.UncostedCacheWriteTokens,
		},
	}
	chain.Runs.Terminal, chain.Runs.Metered = int(row.TerminalRuns), int(row.MeteredRuns)
	chain.Runs.Unreported, chain.Runs.TotalRunMs = int(row.UnreportedRuns), row.TotalRunMs
	return chain, nil
}

// canReadChainAgent applies the private-agent gate GetChatSession uses to one
// chain member's agent.
func (h *Handler) canReadChainAgent(ctx context.Context, agentID pgtype.UUID, actorType, actorID, workspaceID string) bool {
	agent, err := h.Queries.GetAgent(ctx, agentID)
	return err == nil && h.canAccessPrivateAgent(ctx, agent, actorType, actorID, workspaceID)
}
