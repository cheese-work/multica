package handler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// browserLeaseFingerprint prefixes the request_fingerprint of a browser lease decision request.
const browserLeaseFingerprint = serverDecisionFingerprint + "browser_lease:"

const browserLeaseDenyLabel = "Deny"

type browserLeaseDecisions struct {
	h    *Handler
	next browserlease.Decisions
}

// BrowserLeaseDecisions is the decision hook of the browser lease service (CHE-4739, W3-10;
// plan CHE-4695 W1 r3, "Decision gate (F4)"). next judges every answer (W3-11); this hook only
// adds the request when a mint names no decision.
func (h *Handler) BrowserLeaseDecisions(next browserlease.Decisions) browserlease.Decisions {
	return browserLeaseDecisions{h: h, next: next}
}

// Validate delegates to next. When the mint names no decision and next answers decision_required,
// it finds or creates the open request for this lease scope (agent, issue, profile, class,
// duration) and answers decision_required with its ID.
func (d browserLeaseDecisions) Validate(ctx context.Context, s browserlease.Subject) (*time.Time, error) {
	exp, err := d.next.Validate(ctx, s)
	if s.DecisionID.Valid || !errors.Is(err, browserlease.ErrDecisionRequired) {
		return exp, err
	}
	return nil, d.request(ctx, s)
}

func (d browserLeaseDecisions) request(ctx context.Context, s browserlease.Subject) error {
	h := d.h
	issue, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: s.WorkspaceID, ID: s.IssueID})
	if err != nil {
		return err
	}
	agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: s.WorkspaceID, ID: s.AgentID})
	if err != nil {
		return err
	}
	fingerprint := fmt.Sprintf("%s%s:%s:%s:%s:%s", browserLeaseFingerprint,
		uuidToString(s.IssueID), uuidToString(s.AgentID), uuidToString(s.Profile.ID), s.Profile.Class, browserlease.MaxLeaseDuration)

	var found pgtype.UUID
	var created []decisionRequestResult
	err = h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		// The plan takes the workspace lock, so concurrent mints find one request, not two. It asks as
		// the minting agent: the validator accepts only a decision that agent asked.
		plan, err := h.planRunBudgetDecisionsAs(ctx, q, issue, agent, []pgtype.UUID{s.UserID})
		if err != nil {
			return err
		}
		if len(plan.lanes) == 0 {
			return errors.New("browser lease decision: the accountable user is not a member of the workspace")
		}
		open, err := q.FindOpenServerDecisionRequest(ctx, db.FindOpenServerDecisionRequestParams{
			WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, HumanUserID: s.UserID, Fingerprint: fingerprint,
		})
		if err == nil {
			found = open.ID
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// The comment takes the issue lock, after the decision locks.
		notice, err := q.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
			AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "system",
			Content: fmt.Sprintf("Browser lease: agent %s asked for the %s browser profile %q (class %s, up to %s). One decision request asks the accountable user.",
				agent.Name, s.Profile.Class, s.Profile.Name, s.Profile.Class, leaseDurationText()),
		})
		if err != nil {
			return err
		}
		workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
		if err != nil {
			return err
		}
		ask := browserLeaseAsk(workspace.Slug, issue, agent, s.Profile)
		ask.SourceCommentID = uuidToString(notice.ID)
		created, err = h.openRunBudgetAsk(ctx, q, issue, ask, fingerprint, plan)
		return err
	})
	if err != nil {
		return err
	}
	h.publishRunBudgetDecisions(issue, created)
	if len(created) > 0 {
		found = created[0].Request.ID
	}
	return browserlease.DecisionRequiredError{RequestID: found}
}

func leaseDurationText() string {
	return fmt.Sprintf("%d hours", int(browserlease.MaxLeaseDuration/time.Hour))
}

func browserLeaseAsk(slug string, issue db.Issue, agent db.Agent, profile db.BrowserProfile) decisiontx.Ask {
	duration := leaseDurationText()
	// A mention link would start a run of the agent; a plain issue link only points.
	issueLink := fmt.Sprintf("[%s](/%s/issues/%s)", issue.Title, slug, uuidToString(issue.ID))
	scope := fmt.Sprintf("agent %s, issue %s, profile %q, class %s, up to %s", agent.Name, issueLink, profile.Name, profile.Class, duration)
	return decisiontx.Ask{
		ContextELI10: fmt.Sprintf("Agent %s wants a browser that is logged in as profile %q, to work on %s. A browser with a login can read and change that account.", agent.Name, profile.Name, issueLink),
		AgentDid:     "The agent asked Multica for a browser lease. Multica started no browser and holds the lease until you answer. Scope: " + scope + ".",
		Decision: browserlease.LinkedScopeDecision(browserlease.Scope{
			AgentID: agent.ID, IssueID: issue.ID, ProfileID: profile.ID, Class: profile.Class, Duration: browserlease.MaxLeaseDuration,
		}, browserlease.ScopeNames{Slug: slug, Agent: agent.Name, Issue: issue.Title, Profile: profile.Name}),
		StakesIfWrong: "If you allow the wrong agent, issue or profile, the browser can act with that login and send authenticated requests. If you deny a lease the agent needs, the work waits.",
		Options: []decisiontx.AskOption{
			{Label: browserlease.ApproveLabel, WhatHappens: "Multica may grant this one lease for the scope above. The lease ends at the duration or earlier, and you can revoke it.", Pros: []string{"The agent can finish the browser work"}, Cons: []string{"The browser acts with the login for the whole lease"}, CostOrTime: "Up to " + duration + " of browser time"},
			{Label: browserLeaseDenyLabel, WhatHappens: "Multica refuses the lease. The agent gets no browser and no login.", Pros: []string{"The login stays unused"}, Cons: []string{"The browser work does not run"}, CostOrTime: "Free"},
		},
		Recommendation:   "Allow only if you expect this agent to use this login on this issue now.",
		DefaultIfNoReply: "No lease. The agent's mint stays refused and no browser starts.",
		SourceIssueID:    uuidToString(issue.ID),
	}
}
