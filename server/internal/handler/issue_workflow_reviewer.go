package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/capabilityrouting"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The reviewer handler (CHE-7678, step 13 of CHE-7658) routes a step to a
// reviewer who is independent of every author of the issue: an agent whose lab
// is known, differs from every author lab, and who is not itself an author.
// Authors and their labs come from the workflow_role evidence (CHE-7676); a
// reviewer's lab comes from its agent row (model, else runtime provider) through
// service.LabOf, the merge gate's lab function.
// This is routing only: the verdict-time check is a separate step.

const reviewIndependenceCode = "review_independence"

// ReviewIndependenceError refuses a move whose reviewer is not independent of
// the issue's authors. The issue is left unchanged.
type ReviewIndependenceError struct {
	Reason string
	// AuthorLabs are the distinct author labs, sorted; "unknown" when an
	// author's lab is not in the registry.
	AuthorLabs []string
	// ReviewerLab and ReviewerID name the refused reviewer; empty when the
	// refusal is that the table lists no qualified one.
	ReviewerLab string
	ReviewerID  string
}

func (e *ReviewIndependenceError) Error() string {
	return fmt.Sprintf("review independence: %s (author labs: %s)", e.Reason, strings.Join(e.AuthorLabs, ", "))
}

// reviewerLab is the lab of an agent by its runtime provider and model, from
// the same function the merge gate uses (service.LabOf), so it needs no
// operator registry.
func reviewerLab(provider, model string) string { return service.LabOf(model, provider) }

// reviewerGuard is the author history one move is judged against.
type reviewerGuard struct {
	authorIDs  map[string]bool
	authorLabs []string
}

func (h *Handler) loadReviewerGuard(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (*reviewerGuard, error) {
	authors, err := service.IssueAuthors(ctx, q, issueID, reviewerLab)
	if err != nil {
		return nil, err
	}
	g := &reviewerGuard{authorIDs: map[string]bool{}}
	for _, a := range authors {
		g.authorIDs[a.AgentID] = true
		if !slices.Contains(g.authorLabs, a.Lab) {
			g.authorLabs = append(g.authorLabs, a.Lab)
		}
	}
	slices.Sort(g.authorLabs)
	return g, nil
}

// check refuses one reviewer, or returns nil when it is independent.
func (g *reviewerGuard) check(ctx context.Context, q *db.Queries, agent db.Agent) *ReviewIndependenceError {
	id := util.UUIDToString(agent.ID)
	refuse := func(lab, reason string) *ReviewIndependenceError {
		return &ReviewIndependenceError{Reason: reason, AuthorLabs: g.authorLabs, ReviewerLab: lab, ReviewerID: id}
	}
	lab := service.LabUnknown
	if agent.RuntimeID.Valid {
		if rt, err := q.GetAgentRuntime(ctx, agent.RuntimeID); err == nil {
			if l := reviewerLab(rt.Provider, agent.Model.String); l != "" {
				lab = l
			}
		}
	}
	switch {
	case g.authorIDs[id]:
		return refuse(lab, "the reviewer is an author of this issue")
	case lab == service.LabUnknown:
		return refuse(lab, "the reviewer's lab is unknown")
	case slices.Contains(g.authorLabs, service.LabUnknown):
		return refuse(lab, "an author's lab is unknown")
	case slices.Contains(g.authorLabs, lab):
		return refuse(lab, "the reviewer is of an author's lab")
	}
	return nil
}

// pickReviewer is the first reachable reviewer of the issue's routing entry
// (its type's, else the "default" entry for an untyped or unlisted title) that
// passes the independence check. found is false with a refusal that names the
// blocker when the workspace lists no reviewer for it or none qualifies.
func (h *Handler) pickReviewer(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, title string, g *reviewerGuard) (id pgtype.UUID, found bool, refusal error) {
	typ, _ := issueworkflow.TitleType(title)
	ws, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	var settings map[string]any
	_ = json.Unmarshal(ws.Settings, &settings)
	entry, listed := capabilityrouting.FromSettings(settings).For(typ)
	if !listed {
		return pgtype.UUID{}, false, &ReviewIndependenceError{
			Reason:     fmt.Sprintf("blocker: the workspace routes no reviewer for type %q and has no %q routing entry", typ, capabilityrouting.DefaultKey),
			AuthorLabs: g.authorLabs,
		}
	}
	var first *ReviewIndependenceError
	for _, candidate := range entry.Reviewers {
		agentID, err := util.ParseUUID(candidate)
		if err != nil {
			continue
		}
		agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: workspaceID})
		if err != nil || agent.ArchivedAt.Valid || !service.BackgroundAgentReachable(ctx, q, bgagents.Routing, workspaceID, candidate) {
			continue
		}
		refused := g.check(ctx, q, agent)
		if refused == nil {
			return agentID, true, nil
		}
		if first == nil {
			first = refused
		}
	}
	if first != nil {
		first.Reason = fmt.Sprintf("blocker: no reachable reviewer of another lab is listed for type %q; first candidate refused: %s", typ, first.Reason)
		return pgtype.UUID{}, false, first
	}
	return pgtype.UUID{}, false, &ReviewIndependenceError{
		Reason:     fmt.Sprintf("blocker: no reachable reviewer of another lab is listed for type %q", typ),
		AuthorLabs: g.authorLabs,
	}
}

// holdsReviewerRule reports whether the assignee of a step is held to the
// independence rule: a reviewer step, and a same_as step that is the review
// step (agent_review), whose resolved agent could be the author (RGS-1).
func holdsReviewerRule(step issueworkflow.Step) bool {
	return step.Handler.Type == issueworkflow.HandlerReviewer ||
		(step.Handler.Type == issueworkflow.HandlerSameAs && step.StatusKey == statusAgentReview)
}

// checkReviewerAssignee holds the effective assignee of a reviewer step, after
// override and sticky, to the same rule as the table's pick. A person is
// outside the rule; a squad is judged by its leader.
func (h *Handler) checkReviewerAssignee(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, g *reviewerGuard, assignee issueworkflow.StepAssignee) error {
	agentID := assignee.ID
	switch assignee.Type {
	case issueworkflow.HandlerMember:
		return nil
	case issueworkflow.HandlerSquad:
		squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: assignee.ID, WorkspaceID: workspaceID})
		if err != nil {
			return err
		}
		agentID = squad.LeaderID
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: workspaceID})
	if err != nil {
		return &ReviewIndependenceError{Reason: "the reviewer is not an agent of this workspace", AuthorLabs: g.authorLabs, ReviewerLab: service.LabUnknown}
	}
	if refusal := g.check(ctx, q, agent); refusal != nil {
		return refusal
	}
	return nil
}

// checkTouchedReviewer holds an assignee named in the same write that moves the
// issue into a reviewer step to the independence rule. The write is a person's
// or agent's explicit choice, so no handoff is computed, but it must not put an
// author or a same-lab agent on the review.
func (h *Handler) checkTouchedReviewer(ctx context.Context, q *db.Queries, prev db.Issue, params *db.UpdateIssueParams, def *issueworkflow.Definition) error {
	step, ok := def.Step(params.Status.String)
	if !ok || !holdsReviewerRule(step) || !params.AssigneeType.Valid || !params.AssigneeID.Valid {
		return nil
	}
	guard, err := h.loadReviewerGuard(ctx, q, prev.ID)
	if err != nil {
		return err
	}
	return h.checkReviewerAssignee(ctx, q, prev.WorkspaceID, guard, issueworkflow.StepAssignee{Type: params.AssigneeType.String, ID: params.AssigneeID})
}
