package handler

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A delegated decision shows "<agent name> for <human in charge name>", computed
// at read time from the workspace registration and resolveHumanInCharge, never
// stored and never a constant (CHE-8794 S4b). The stored decider value stays
// the raw legacy label.
const (
	deciderDisplayAgentFallback = "delegate agent"
	deciderDisplayHumanFallback = "the workspace owner"
)

// deciderDisplayer resolves the registered delegate once per request and the
// human in charge once per issue.
type deciderDisplayer struct {
	h      *Handler
	ctx    context.Context
	agent  *db.Agent
	humans map[string]string
}

func (h *Handler) newDeciderDisplayer(ctx context.Context, workspaceID pgtype.UUID) *deciderDisplayer {
	d := &deciderDisplayer{h: h, ctx: ctx, humans: map[string]string{}}
	agentID, _, err := agentDecisionJevSetting(ctx, h.DB, workspaceID)
	if err != nil {
		slog.Warn("decider display: read registration", "error", err)
		return d
	}
	id, err := parseUUIDStrict(agentID)
	if err != nil {
		return d
	}
	agent, err := h.Queries.GetAgent(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("decider display: read agent", "error", err)
		}
		return d
	}
	if agent.WorkspaceID == workspaceID {
		d.agent = &agent
	}
	return d
}

// display returns the line for a record on issueID. Without the registered
// agent nobody is asking, so the human falls back too.
func (d *deciderDisplayer) display(issueID string) string {
	if d.agent == nil {
		return deciderDisplayAgentFallback + " for " + deciderDisplayHumanFallback
	}
	human, ok := d.humans[issueID]
	if !ok {
		human = d.resolveHuman(issueID)
		d.humans[issueID] = human
	}
	return d.agent.Name + " for " + human
}

func (d *deciderDisplayer) resolveHuman(issueID string) string {
	id, err := parseUUIDStrict(issueID)
	if err != nil {
		return deciderDisplayHumanFallback
	}
	issue, err := d.h.Queries.GetIssue(d.ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("decider display: read issue", "error", err)
		}
		return deciderDisplayHumanFallback
	}
	human, err := d.h.resolveHumanInCharge(d.ctx, d.h.Queries, issue, *d.agent)
	if err != nil {
		slog.Warn("decider display: resolve human in charge", "error", err)
		return deciderDisplayHumanFallback
	}
	if human == nil || human.Name == "" {
		return deciderDisplayHumanFallback
	}
	return human.Name
}
