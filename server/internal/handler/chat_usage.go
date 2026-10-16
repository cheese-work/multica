package handler

import (
	"encoding/json"
	"net/http"
)

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
	writeJSON(w, http.StatusOK, resp)
}
