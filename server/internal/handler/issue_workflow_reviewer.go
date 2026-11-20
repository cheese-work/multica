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
// reviewer's lab is predicted from its agent row (runtime provider and model).
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

// reviewerGuard is the author history one move is judged against.
type reviewerGuard struct {
	authorIDs  map[string]bool
	authorLabs []string
}

func (h *Handler) loadReviewerGuard(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (*reviewerGuard, error) {
	authors, err := service.IssueAuthors(ctx, q, issueID, voteLab)
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
			if l := voteLab(rt.Provider, agent.Model.String); l != "" {
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

// pickReviewer is the first reachable reviewer of the issue type's routing
// entry that passes the independence check. found is false with a nil refusal
// when the workspace routes this type nowhere, and false with a refusal when
// it lists reviewers but none qualifies.
func (h *Handler) pickReviewer(ctx context.Context, q *db.Queries, issue db.Issue, g *reviewerGuard) (id pgtype.UUID, found bool, refusal error) {
	typ, ok := issueworkflow.TitleType(issue.Title)
	if !ok {
		return pgtype.UUID{}, false, nil
	}
	ws, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	var settings map[string]any
	_ = json.Unmarshal(ws.Settings, &settings)
	entry, listed := capabilityrouting.FromSettings(settings)[typ]
	if !listed {
		return pgtype.UUID{}, false, nil
	}
	var first *ReviewIndependenceError
	for _, candidate := range entry.Reviewers {
		agentID, err := util.ParseUUID(candidate)
		if err != nil {
			continue
		}
		agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: issue.WorkspaceID})
		if err != nil || agent.ArchivedAt.Valid || !service.BackgroundAgentReachable(ctx, q, bgagents.Routing, issue.WorkspaceID, candidate) {
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
		first.Reason = fmt.Sprintf("no reachable reviewer of another lab is listed for type %q; first candidate refused: %s", typ, first.Reason)
		return pgtype.UUID{}, false, first
	}
	return pgtype.UUID{}, false, &ReviewIndependenceError{
		Reason:     fmt.Sprintf("no reachable reviewer of another lab is listed for type %q", typ),
		AuthorLabs: g.authorLabs,
	}
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
