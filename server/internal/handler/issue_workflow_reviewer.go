package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/capabilityrouting"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/reviewindependence"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The reviewer handler (CHE-7678, step 13 of CHE-7658) routes a step to a
// reviewer who is independent of every author of the issue: an agent whose
// value is known, differs from every author value, and who is not itself an
// author. Values come from service.ReviewValues, the resolver the merge gate also
// uses: the workspace setting review_independence, else service.LabOf (CHE-9319).
// With the setting present the authors are the values on the linked PRs'
// commits; otherwise they are the agents that ran at a work step (CHE-7676).
// This is routing only: the verdict-time check is a separate step.

const reviewIndependenceCode = "review_independence"

// ReviewIndependenceError refuses a move whose reviewer is not independent of
// the issue's authors. The issue is left unchanged.
type ReviewIndependenceError struct {
	Reason string
	// Attribute names the compared attribute, for example "lab".
	Attribute string
	// AuthorValues are the distinct author values, sorted; "unknown" when an
	// author has none.
	AuthorValues []string
	// Evidence names, per author value, the commits or runs that made it an
	// author (CHE-9319).
	Evidence map[string][]string
	// ReviewerValue and ReviewerID name the refused reviewer; empty when the
	// refusal is that the table lists no qualified one.
	ReviewerValue string
	ReviewerID    string
}

func (e *ReviewIndependenceError) Error() string {
	msg := fmt.Sprintf("review independence: %s (author %s values: %s)", e.Reason, e.Attribute, strings.Join(e.AuthorValues, ", "))
	var parts []string
	for _, value := range e.AuthorValues {
		if ev := e.Evidence[value]; len(ev) > 0 {
			parts = append(parts, value+": "+strings.Join(ev, ", "))
		}
	}
	if len(parts) > 0 {
		msg += "; author evidence: " + strings.Join(parts, "; ")
	}
	return msg
}

// maxEvidence caps the evidence listed per lab in a refusal.
const maxEvidence = 3

func addEvidence(ev map[string][]string, value, what string) {
	if n := len(ev[value]); n < maxEvidence {
		ev[value] = append(ev[value], what)
	} else if n == maxEvidence {
		ev[value] = append(ev[value], "and more")
	}
}

// reviewerGuard is the author history one move is judged against.
type reviewerGuard struct {
	values       service.ReviewValues
	authorIDs    map[string]bool
	authorValues []string
	evidence     map[string][]string
}

func (g *reviewerGuard) refusal(reason string) *ReviewIndependenceError {
	return &ReviewIndependenceError{Reason: reason, Attribute: g.values.Attribute(), AuthorValues: g.authorValues, Evidence: g.evidence}
}

// prCommits reads the commits of one linked PR: the test seam, else the GitHub
// App client. An error means the commits are unknown.
func (h *Handler) prCommits(ctx context.Context, installationID int64, owner, repo string, number int32) ([]reviewindependence.PRCommit, error) {
	if h.readPRCommits != nil {
		return h.readPRCommits(ctx, installationID, owner, repo, number)
	}
	if !h.PRRefresh.Enabled() {
		return nil, errors.New("github: snapshot API is not configured")
	}
	commits, err := h.PRRefresh.Client().PRCommits(ctx, installationID, owner, repo, number)
	if err != nil {
		return nil, err
	}
	out := make([]reviewindependence.PRCommit, len(commits))
	for i, c := range commits {
		out[i] = reviewindependence.PRCommit(c)
	}
	return out, nil
}

// linkedPRCommits is the commits of every PR linked to the issue, nil when the
// setting is absent, the issue has no linked PR, or a commit list cannot be read:
// the author set then keeps the run-based rule (fail closed).
func (h *Handler) linkedPRCommits(ctx context.Context, q *db.Queries, rv service.ReviewValues, issueID pgtype.UUID) []reviewindependence.PRCommit {
	if !rv.Configured() {
		return nil
	}
	prs, err := q.ListPullRequestsByIssue(ctx, issueID)
	if err != nil || len(prs) == 0 {
		return nil
	}
	var all []reviewindependence.PRCommit
	for _, pr := range prs {
		commits, err := h.prCommits(ctx, pr.InstallationID, pr.RepoOwner, pr.RepoName, pr.PrNumber)
		if err != nil || len(commits) == 0 {
			return nil
		}
		all = append(all, commits...)
	}
	return all
}

// loadReviewerGuard builds the author history of an issue (CHE-9319) from the
// one author computation the merge gate also uses (service.IssueAuthorSet).
func (h *Handler) loadReviewerGuard(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (*reviewerGuard, error) {
	issue, err := q.GetIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	ws, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return nil, err
	}
	rv := service.ReviewValuesFromSettings(ws.Settings)
	set, err := rv.IssueAuthorSet(ctx, q, issueID, h.linkedPRCommits(ctx, q, rv, issueID))
	if err != nil {
		return nil, err
	}
	g := &reviewerGuard{values: rv, authorIDs: set.AgentIDs, authorValues: set.Values, evidence: map[string][]string{}}
	for value, evidence := range set.Evidence {
		for _, e := range evidence {
			addEvidence(g.evidence, value, e)
		}
	}
	return g, nil
}

// check refuses one reviewer, or returns nil when it is independent.
func (g *reviewerGuard) check(ctx context.Context, q *db.Queries, agent db.Agent) *ReviewIndependenceError {
	id := util.UUIDToString(agent.ID)
	refuse := func(value, reason string) *ReviewIndependenceError {
		e := g.refusal(reason)
		e.ReviewerValue, e.ReviewerID = value, id
		return e
	}
	lab := service.LabUnknown
	if agent.RuntimeID.Valid {
		if rt, err := q.GetAgentRuntime(ctx, agent.RuntimeID); err == nil {
			if l := g.values.AgentValue(rt.Provider, agent.Model.String); l != "" {
				lab = l
			}
		}
	}
	attr := g.values.Attribute()
	switch {
	case g.authorIDs[id]:
		return refuse(lab, "the reviewer is an author of this issue")
	case lab == service.LabUnknown:
		return refuse(lab, fmt.Sprintf("the reviewer's %s is unknown", attr))
	case slices.Contains(g.authorValues, service.LabUnknown):
		return refuse(lab, fmt.Sprintf("an author's %s is unknown", attr))
	case slices.Contains(g.authorValues, lab):
		return refuse(lab, fmt.Sprintf("the reviewer is of an author's %s", attr))
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
		return pgtype.UUID{}, false, g.refusal(fmt.Sprintf("blocker: the workspace routes no reviewer for type %q and has no %q routing entry", typ, capabilityrouting.DefaultKey))
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
		first.Reason = fmt.Sprintf("blocker: no reachable reviewer of another %s is listed for type %q; first candidate refused: %s", g.values.Attribute(), typ, first.Reason)
		return pgtype.UUID{}, false, first
	}
	return pgtype.UUID{}, false, g.refusal(fmt.Sprintf("blocker: no reachable reviewer of another %s is listed for type %q", g.values.Attribute(), typ))
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
		e := g.refusal("the reviewer is not an agent of this workspace")
		e.ReviewerValue = service.LabUnknown
		return e
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
func (h *Handler) checkTouchedReviewer(ctx context.Context, q *db.Queries, prev db.Issue, params *db.UpdateIssueParams, def *issueworkflow.Definition, callerIsAgent bool) error {
	step, ok := def.Step(params.Status.String)
	if !ok || !holdsReviewerRule(step) || !params.AssigneeType.Valid || !params.AssigneeID.Valid {
		return nil
	}
	guard, err := h.loadReviewerGuard(ctx, q, prev.ID)
	if err != nil {
		return err
	}
	assignee := issueworkflow.StepAssignee{Type: params.AssigneeType.String, ID: params.AssigneeID}
	if err := h.checkReviewerAssignee(ctx, q, prev.WorkspaceID, guard, assignee); err != nil {
		return err
	}
	return h.checkReviewerScope(ctx, q, prev, textOr(params.Title, prev.Title), assignee, callerIsAgent)
}
