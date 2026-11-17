package handler

import (
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

type needsYouKinds struct {
	Decision int `json:"decision"`
	Ask      int `json:"ask"`
	Later    int `json:"later"`
	Unseen   int `json:"unseen"`
}

type needsYouResponse struct {
	AsOf         time.Time      `json:"as_of"`
	Total        int            `json:"total"`
	ByKind       needsYouKinds  `json:"by_kind"`
	OutsideChats map[string]int `json:"outside_chats"`
	Snoozed      needsYouKinds  `json:"snoozed"`
	WithAgent    int            `json:"with_agent"`
	Unclassified int            `json:"unclassified"`
	NextChangeAt *time.Time     `json:"next_change_at"`
}

func needsYouKindsOf(c service.AttentionCounts) needsYouKinds {
	return needsYouKinds{Decision: c.Decision, Ask: c.Ask, Later: c.Later, Unseen: c.Unseen}
}

// GetNeedsYou returns one member's "needs you" summary (CHE-5670 D6). A member reads only their own;
// only a task token may name another member with ?for=, and must. Counts only: no ask text, no options.
func (h *Handler) GetNeedsYou(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	taskToken := r.Header.Get("X-Actor-Source") == "task_token"
	viewer := member.UserID
	if raw := r.URL.Query().Get("for"); raw != "" || taskToken {
		forID, ok := parseUUIDOrBadRequest(w, raw, "for")
		if !ok {
			return
		}
		if forID != member.UserID && !taskToken {
			writeError(w, http.StatusForbidden, "members read only their own summary")
			return
		}
		viewer = forID
	}
	viewerID := uuidToString(viewer)

	// The viewer's access, not the caller's: a task token reads on behalf of the named member.
	vis, fail := h.attentionVisibilityFor(r.Context(), viewerID, workspaceID, "member", viewerID)
	if fail != nil {
		fail.write(w)
		return
	}
	hidden := make(map[string]bool, len(vis.hidden))
	for _, root := range vis.hidden {
		hidden[uuidToString(root)] = true
	}
	summary, err := service.LoadNeedsYou(r.Context(), h.Queries, member.WorkspaceID, viewer, hidden, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load needs-you summary")
		return
	}
	writeJSON(w, http.StatusOK, needsYouResponse{
		AsOf: summary.AsOf, Total: summary.Total, ByKind: needsYouKindsOf(summary.ByKind),
		OutsideChats: map[string]int{"decision": summary.OutsideChats},
		Snoozed:      needsYouKindsOf(summary.Snoozed), WithAgent: summary.WithAgent,
		Unclassified: summary.Unclassified, NextChangeAt: summary.NextChangeAt,
	})
}
