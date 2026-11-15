package handler

import (
	"context"
	"net/http"
	"strings"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// mentionRange is the byte span of one whole [label](mention://agent/id) match.
type mentionRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// invalidAgentMentionRanges returns the spans of explicit agent mentions that
// name no agent the actor can use: a malformed id, an agent outside the issue's
// workspace, one the actor cannot invoke, or an archived one. The checks and
// their order match the direct-agent branch of
// resolveMentionedAgentCommentTriggers, so this agrees with what the composer
// preview reports. Offline runtimes, budgets and trigger policy are not
// checked: those stay non-fatal blocked outcomes. skip holds lower-case agent
// ids that need no check (mentions already present before an edit).
func (h *Handler) invalidAgentMentionRanges(ctx context.Context, issue db.Issue, content, actorType, actorID, originatorUserID string, skip map[string]struct{}) []mentionRange {
	wsID := uuidToString(issue.WorkspaceID)
	var invalid []mentionRange
	// Verdict per canonical agent id: a comment can repeat one mention many
	// times, and each check costs database queries.
	verdicts := map[string]bool{}
	for _, m := range util.MentionRe.FindAllStringSubmatchIndex(content, -1) {
		if content[m[4]:m[5]] != "agent" {
			continue
		}
		id := content[m[6]:m[7]]
		if _, ok := skip[strings.ToLower(id)]; ok {
			continue
		}
		key := canonicalMentionID(id)
		usable, seen := verdicts[key]
		if !seen {
			usable = h.agentMentionUsable(ctx, issue, id, actorType, actorID, originatorUserID, wsID)
			verdicts[key] = usable
		}
		if !usable {
			invalid = append(invalid, mentionRange{Start: m[0], End: m[1]})
		}
	}
	return invalid
}

// canonicalMentionID maps every spelling of one UUID to one key; an id that
// does not parse keeps its lower-cased text and is rejected without a query.
func canonicalMentionID(id string) string {
	if u, err := util.ParseUUID(id); err == nil {
		return util.UUIDToString(u)
	}
	return strings.ToLower(id)
}

func (h *Handler) agentMentionUsable(ctx context.Context, issue db.Issue, id, actorType, actorID, originatorUserID, wsID string) bool {
	agentUUID, err := util.ParseUUID(id)
	if err != nil {
		return false
	}
	agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentUUID, WorkspaceID: issue.WorkspaceID})
	if err != nil || !h.canInvokeAgent(ctx, agent, actorType, actorID, originatorUserID, wsID) {
		return false
	}
	return !agent.ArchivedAt.Valid
}

// rejectInvalidAgentMentions writes the 422 and returns true when content holds
// an unusable agent mention. One fixed message for every cause: the body must
// not reveal whether an id exists, is private, or is archived.
func (h *Handler) rejectInvalidAgentMentions(ctx context.Context, w http.ResponseWriter, issue db.Issue, content, actorType, actorID, originatorUserID string, skip map[string]struct{}) bool {
	invalid := h.invalidAgentMentionRanges(ctx, issue, content, actorType, actorID, originatorUserID, skip)
	if len(invalid) == 0 {
		return false
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":            "comment mentions an agent that does not exist or is not available to you",
		"code":             "invalid_agent_mentions",
		"invalid_mentions": invalid,
	})
	return true
}

// agentMentionIDs returns the lower-case agent ids mentioned in content.
func agentMentionIDs(content string) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, m := range util.ParseMentions(content) {
		if m.Type == "agent" {
			ids[strings.ToLower(m.ID)] = struct{}{}
		}
	}
	return ids
}
