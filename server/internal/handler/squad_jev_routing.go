package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/jev"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// tryJevSquadRouting attempts server-side fast-path routing using TypeSafe AI's Jev model.
// When confidence >= minConfidence (default 0.90) and snapshot freshness validates atomically,
// it dispatches the task directly to the selected squad worker, updates status (backlog/todo -> in_progress),
// writes an announcement comment (type='jev_route'), and records squad activity.
// On confidence < minConfidence or error, it returns (false, advisory) where advisory contains
// pre-computed recommendations to append to the squad leader's handoff note.
// In shadow mode (CHE-1745) it only records the Jev decision as a structured log line and
// returns (false, ""), leaving the leader path exactly as with routing off.
func (h *Handler) tryJevSquadRouting(ctx context.Context, issue db.Issue, squad db.Squad, authorType, authorID, leaderOriginator string, triggerCommentID ...pgtype.UUID) (bool, string) {
	// Sol directive & Sonnet Blocker 2 & CHE-1711: Do not route agent-authored, system-authored,
	// or unknown-authored comments through Jev.
	// Worker-result returns and agent comment replies stay with the squad leader.
	// When comment read fails or author is unknown/non-member, bypass Jev to preserve normal leader fallback.
	// Authorized agent-created issues and agent-originated squad assignments (non-comment paths)
	// are preserved for Jev routing.
	if len(triggerCommentID) > 0 && triggerCommentID[0].Valid && authorType != "member" {
		return false, ""
	}

	config, err := loadGovernanceControl(ctx, h.DB, issue.WorkspaceID)
	if err != nil {
		return false, ""
	}
	mode := config.Settings.SquadRoutingMode()
	if mode == governance.SquadRoutingOff {
		return false, ""
	}

	decisionID := fmt.Sprintf("jd_%s", strings.ReplaceAll(dbid.NewV7().String(), "-", "")[24:])
	if mode == governance.SquadRoutingShadow {
		h.startJevShadowRouting(ctx, issue, squad, authorType, authorID, config.Settings, decisionID, triggerCommentID...)
		return false, ""
	}

	decision, reason := h.evaluateJevSquadRouting(ctx, issue, squad, authorType, authorID, config.Settings, decisionID)
	if reason != "" {
		return false, jevPreRouteAdvisory(decisionID, reason)
	}
	minConfidence := jevMinConfidence(config.Settings)
	winnerOption, confidence := decision.winnerOption, decision.confidence
	runnerUpProb := decision.runnerUpProb
	winnerLabel, runnerUpLabel := decision.winnerLabel, decision.runnerUpLabel
	candidateMap, tiers := decision.candidateMap, decision.tiers

	if winnerOption == jevEscalateOption || confidence < minConfidence {
		ambiguity := fmt.Sprintf("Confidence %.2f is below %.2f threshold for automated dispatch", confidence, minConfidence)
		if winnerOption == jevEscalateOption {
			ambiguity = "Jev recommended escalation to squad leader"
		}
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: %s · Conf %.2f\n- Runner-up: %s · Conf %.2f\n- Ambiguity: %s\n- Prior State Changes: none\n- Decision ID: %s",
			winnerLabel, confidence, runnerUpLabel, runnerUpProb, ambiguity, decisionID)
		return false, advisory
	}

	winnerAgent := candidateMap[winnerOption]
	winnerTier := tiers[winnerOption]

	// Access Gate: verify actor is allowed to invoke the chosen worker agent
	if !h.canInvokeAgent(ctx, winnerAgent, authorType, authorID, leaderOriginator, uuidToString(issue.WorkspaceID)) {
		slog.Warn("jev squad routing: actor cannot invoke selected worker", "actor", authorID, "agent", winnerAgent.Name)
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: %s · Conf %.2f\n- Runner-up: %s · Conf %.2f\n- Ambiguity: Actor not permitted to invoke selected specialist agent %s\n- Prior State Changes: none\n- Decision ID: %s",
			winnerLabel, confidence, runnerUpLabel, runnerUpProb, winnerAgent.Name, decisionID)
		return false, advisory
	}

	// Dedup: check if chosen worker agent already has a pending task for this issue
	hasPendingWorker, err := h.Queries.HasPendingTaskForIssueAndAgent(ctx, db.HasPendingTaskForIssueAndAgentParams{
		IssueID: issue.ID,
		AgentID: winnerAgent.ID,
		HeadSha: h.TaskService.ResolveIssueReviewSHAParam(ctx, issue.ID),
	})
	if err != nil || hasPendingWorker {
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: %s · Conf %.2f\n- Runner-up: %s · Conf %.2f\n- Ambiguity: Selected specialist %s already has a pending task for this issue\n- Prior State Changes: none\n- Decision ID: %s",
			winnerLabel, confidence, runnerUpLabel, runnerUpProb, winnerAgent.Name, decisionID)
		return false, advisory
	}

	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		slog.Error("jev squad routing: tx begin error", "error", err)
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: %s · Conf %.2f\n- Runner-up: %s · Conf %.2f\n- Ambiguity: Database transaction failed: %v\n- Prior State Changes: none\n- Decision ID: %s", winnerLabel, confidence, runnerUpLabel, runnerUpProb, err, decisionID)
		return false, advisory
	}
	defer tx.Rollback(ctx)

	qtx := h.Queries.WithTx(tx)

	// Routing writes the issue's status, so it takes the same locks as every
	// issue write in a project, in the same order: the status catalog, the
	// project, then the issue row. A workflow switch or edit cannot commit
	// between the membership check below and the write.
	if issue.ProjectID.Valid || issue.WorkflowID.Valid {
		if err := qtx.LockIssueStatusCatalogShared(ctx, issue.WorkspaceID); err != nil {
			return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Failed to lock the status catalog: %v\n- Decision ID: %s", err, decisionID)
		}
	}
	if issue.ProjectID.Valid {
		if _, err := qtx.LockProjectForIssueWrite(ctx, db.LockProjectForIssueWriteParams{ID: issue.ProjectID, WorkspaceID: issue.WorkspaceID}); err != nil {
			return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Failed to lock the issue's project: %v\n- Decision ID: %s", err, decisionID)
		}
	}

	var currentRevision int64
	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT revision, status FROM issue WHERE id = $1 FOR NO KEY UPDATE`, issue.ID).Scan(&currentRevision, &currentStatus)
	if err != nil {
		return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Failed to lock issue: %v\n- Decision ID: %s", err, decisionID)
	}

	if currentRevision != issue.Revision {
		slog.Warn("jev squad routing: revision conflict", "expected", issue.Revision, "actual", currentRevision)
		return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Issue revision changed during evaluation (expected %d, found %d)\n- Prior State Changes: none\n- Decision ID: %s", issue.Revision, currentRevision, decisionID)
	}

	// A named workflow may leave in_progress out. Routing then dispatches the
	// worker and keeps the status, so the issue never sits outside its
	// project's workflow. The revision check above means the project is the one
	// locked.
	statusChanged := false
	newStatus := currentStatus
	if currentStatus == "backlog" || currentStatus == "todo" {
		workflow, _, err := issueworkflow.ForIssue(ctx, qtx, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
		if err != nil {
			return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Failed to read the project workflow: %v\n- Decision ID: %s", err, decisionID)
		}
		if issueworkflow.CheckStatus(workflow, issuestatus.InProgress) == nil {
			newStatus = issuestatus.InProgress
			statusChanged = true
		}
	}

	var updatedIssue db.Issue
	if statusChanged {
		updatedIssue, err = qtx.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
			ID:          issue.ID,
			Status:      newStatus,
			WorkspaceID: issue.WorkspaceID,
		})
		if err != nil {
			return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Failed to update issue status: %v\n- Decision ID: %s", err, decisionID)
		}
	}

	commentID := dbid.NewV7()
	commentContent := fmt.Sprintf("⚡ Jev routed to %s (Tier %s) · Conf %.2f · Reason: High-confidence specialist match · Decision %s", winnerAgent.Name, winnerTier, confidence, decisionID)

	commentRow, err := qtx.CreateComment(ctx, db.CreateCommentParams{
		ID:          commentID,
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     commentContent,
		Type:        "jev_route",
	})
	if err != nil {
		return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Failed to create routing announcement: %v\n- Decision ID: %s", err, decisionID)
	}

	detailsJSON, _ := json.Marshal(map[string]any{
		"squad_id":    uuidToString(squad.ID),
		"decision_id": decisionID,
		"winner_id":   uuidToString(winnerAgent.ID),
		"winner_name": winnerAgent.Name,
		"winner_tier": winnerTier,
		"confidence":  confidence,
		"outcome":     "action",
		"via":         "jev",
	})
	activity, err := qtx.CreateActivity(ctx, db.CreateActivityParams{
		ID:          dbid.NewV7(),
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
		ActorType:   pgtype.Text{String: "system", Valid: true},
		ActorID:     pgtype.UUID{},
		Action:      "squad_leader_evaluated",
		Details:     detailsJSON,
	})
	if err != nil {
		return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Failed to record squad activity: %v\n- Decision ID: %s", err, decisionID)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Commit failed: %v\n- Decision ID: %s", err, decisionID)
	}

	if statusChanged {
		issue.Status = newStatus
		issue.Revision = updatedIssue.Revision
		prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
		resp := issueToResponse(updatedIssue, prefix)
		h.fillStatusCategory(ctx, updatedIssue.WorkspaceID, &resp)
		h.publish(protocol.EventIssueUpdated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
			"issue":          resp,
			"status_changed": true,
			"prev_status":    currentStatus,
			"creator_type":   issue.CreatorType,
			"creator_id":     uuidToString(issue.CreatorID),
			"source":         "jev_route",
		})
	}

	commentEvent := map[string]any{
		"comment":        commentToResponse(commentRow.Comment(), nil, nil),
		"issue_title":    issue.Title,
		"issue_status":   newStatus,
		"issue_revision": commentRow.IssueRevision,
	}
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", commentEvent)

	h.publish(protocol.EventActivityCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"issue_id": uuidToString(issue.ID),
		"entry": map[string]any{
			"type":       "activity",
			"id":         uuidToString(activity.ID),
			"actor_type": "system",
			"actor_id":   "",
			"action":     activity.Action,
			"details":    json.RawMessage(detailsJSON),
			"created_at": timestampToString(activity.CreatedAt),
		},
	})

	var trigID pgtype.UUID
	if len(triggerCommentID) > 0 {
		trigID = triggerCommentID[0]
	}
	if _, err = h.TaskService.EnqueueTaskForMention(ctx, issue, winnerAgent.ID, trigID, service.OriginDerived); err != nil {
		slog.Error("jev dispatch: failed to enqueue member task, returning advisory to wake leader",
			"issue_id", uuidToString(issue.ID),
			"agent_id", uuidToString(winnerAgent.ID),
			"error", err)
		var priorParts []string
		if statusChanged {
			priorParts = append(priorParts, fmt.Sprintf("status -> %s (retained)", newStatus))
		}
		priorParts = append(priorParts, "jev_route comment (retained)", "outcome=action activity (retained)")
		priorChanges := strings.Join(priorParts, ", ")
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: %s · Conf %.2f\n- Runner-up: %s · Conf %.2f\n- Ambiguity: Jev selected %s (Tier %s, Conf %.2f), but automated task enqueue failed: %v\n- Prior State Changes: %s\n- Decision ID: %s",
			winnerLabel, confidence, runnerUpLabel, runnerUpProb, winnerAgent.Name, winnerTier, confidence, err, priorChanges, decisionID)
		return false, advisory
	}

	return true, ""
}

const (
	jevEscalateOption = "escalate_to_leader"
	jevShadowMessage  = "jev squad shadow decision"
)

// jevDecision is one Jev answer for a squad routing question, before any
// confidence threshold, access gate or dispatch is applied to it.
type jevDecision struct {
	winnerOption   string
	confidence     float64
	runnerUpOption string
	runnerUpProb   float64
	winnerLabel    string
	runnerUpLabel  string
	candidateMap   map[string]db.Agent
	tiers          map[string]string
	telemetry      jevTelemetry
}

// jevTelemetry is the measured provider-side detail of one Jev call, kept so a
// shadow record can be replayed and costed (CHE-1412). It is also returned with
// the reason when a billed call produced no usable decision.
type jevTelemetry struct {
	servingModel string
	inputTokens  int
	outputTokens int
	inputSHA256  string
}

func jevPreRouteAdvisory(decisionID, ambiguity string) string {
	return fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: none\n- Runner-up: none\n- Ambiguity: %s\n- Prior State Changes: none\n- Decision ID: %s", ambiguity, decisionID)
}

func jevMinConfidence(settings governance.WorkspaceSettings) float64 {
	if settings.JevMinConfidence != nil {
		return *settings.JevMinConfidence
	}
	return 0.90
}

// evaluateJevSquadRouting asks Jev which squad member should take the issue.
// It has no side effects on issue state. A non-empty reason means no decision
// was obtained (misconfiguration, provider failure, malformed answer); the
// returned decision may then still carry telemetry of a call that was sent.
func (h *Handler) evaluateJevSquadRouting(ctx context.Context, issue db.Issue, squad db.Squad, authorType, authorID string, settings governance.WorkspaceSettings, decisionID string) (*jevDecision, string) {
	apiURL := strings.TrimSpace(settings.JevAPIURL)
	if apiURL != "" {
		parsedURL, err := url.Parse(apiURL)
		if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
			slog.Warn("jev squad routing: invalid api url", "api_url", apiURL)
			return nil, fmt.Sprintf("Configured Jev API URL is invalid: %s", apiURL)
		}
	}
	if featureflag.EvalContextFrom(ctx).WorkspaceID == "" {
		ctx = featureflag.WithEvalContext(ctx, featureflag.EvalContext{
			WorkspaceID: uuidToString(issue.WorkspaceID),
		})
	}

	var jevClient *jev.Client
	if apiKeyBytes, err := h.ResolveJevCredential(ctx, issue.WorkspaceID); err == nil && len(apiKeyBytes) > 0 {
		opts := jev.Options{
			APIKey:  string(apiKeyBytes),
			BaseURL: apiURL,
			Timeout: 5 * time.Second,
			Gate:    featureflags.JevProductionGate{Flags: h.FeatureFlags},
		}
		jevClient, _ = jev.NewClient(opts)
	} else if h.Jev != nil {
		jevClient = h.Jev
	}

	if jevClient == nil {
		slog.Warn("jev squad routing: credentials unconfigured", "workspace_id", uuidToString(issue.WorkspaceID))
		return nil, "Jev provider credential unavailable"
	}

	members, err := h.Queries.ListSquadMembers(ctx, squad.ID)
	if err != nil {
		return nil, fmt.Sprintf("Failed to load squad members: %v", err)
	}

	candidates := make([]db.Agent, 0, len(members))
	candidateMap := make(map[string]db.Agent, len(members))
	memberRoleMap := make(map[string]string, len(members))
	for _, m := range members {
		if m.MemberType != "agent" || uuidToString(m.MemberID) == uuidToString(squad.LeaderID) || (authorType == "agent" && uuidToString(m.MemberID) == authorID) {
			continue
		}
		agent, err := h.Queries.GetAgent(ctx, m.MemberID)
		if err != nil || agent.ArchivedAt.Valid || !agent.RuntimeID.Valid {
			continue
		}
		candidates = append(candidates, agent)
		cid := uuidToString(agent.ID)
		candidateMap[cid] = agent
		memberRoleMap[cid] = m.Role
	}

	if len(candidates) == 0 {
		return nil, "No eligible active squad member agents with valid runtimes"
	}

	tiers := parseSquadTierList(squad.Instructions, candidates)
	skillsByAgentID, _ := loadSquadMemberSkillNames(ctx, h.Queries, members, uuidToString(squad.LeaderID))

	stateBuilder := jev.NewStateBuilder().Reserve(2000)

	stateBuilder.Add("squad", map[string]any{
		"id":           uuidToString(squad.ID),
		"name":         squad.Name,
		"description":  squad.Description,
		"instructions": squad.Instructions,
		"leader_id":    uuidToString(squad.LeaderID),
	}, jev.PriorityRequired)

	roster := make([]map[string]any, 0, len(candidates))
	for _, cand := range candidates {
		cid := uuidToString(cand.ID)
		roster = append(roster, map[string]any{
			"agent_id":    cid,
			"name":        cand.Name,
			"description": cand.Description,
			"tier":        tiers[cid],
			"skills":      skillsByAgentID[cid],
		})
	}
	stateBuilder.Add("roster", roster, jev.PriorityRequired)

	desc := issue.Description.String
	if jev.EstimateTokens(desc) > 4000 {
		desc = truncateRunes(desc, 12000)
	}
	stateBuilder.Add("issue", map[string]any{
		"id":          uuidToString(issue.ID),
		"title":       issue.Title,
		"description": desc,
		"status":      issue.Status,
		"priority":    issue.Priority,
		"revision":    issue.Revision,
	}, jev.PriorityRequired)

	// Filter and budget comments (up to 14,000 tokens)
	// Spec: Mandatory sections (roster tier table or latest human comment) must fit;
	// if they cannot fit, fail-closed and route to leader.
	type humanCommentData struct {
		ID         string
		AuthorType string
		CreatedAt  string
		Content    string
	}
	var latestHuman *humanCommentData
	var eligible []db.Comment

	if rawComments, err := h.Queries.ListCommentsForIssue(ctx, db.ListCommentsForIssueParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		Limit:       20,
	}); err == nil && len(rawComments) > 0 {
		for _, c := range rawComments {
			if c.Type != "jev_route" && strings.TrimSpace(c.Content) != "" {
				eligible = append(eligible, c)
			}
		}
	}

	latestHumanIdx := -1
	for i := len(eligible) - 1; i >= 0; i-- {
		if eligible[i].AuthorType == "member" || eligible[i].AuthorType == "user" {
			latestHumanIdx = i
			latestHuman = &humanCommentData{
				ID:         uuidToString(eligible[i].ID),
				AuthorType: eligible[i].AuthorType,
				CreatedAt:  timestampToString(eligible[i].CreatedAt),
				Content:    eligible[i].Content,
			}
			break
		}
	}

	// Sonnet finding: If 20 newest comments contain no human comment, check if an older
	// human comment exists on the issue so Jev does not route blind to earlier human instructions.
	if latestHuman == nil {
		var outsideID pgtype.UUID
		var outsideAuthorType string
		var outsideContent string
		var outsideCreatedAt pgtype.Timestamptz
		err := h.DB.QueryRow(ctx, `
			SELECT id, author_type, content, created_at
			FROM comment
			WHERE issue_id = $1 AND workspace_id = $2
			  AND author_type IN ('member', 'user')
			  AND type != 'jev_route'
			  AND TRIM(content) != ''
			ORDER BY created_at DESC, id DESC
			LIMIT 1
		`, issue.ID, issue.WorkspaceID).Scan(&outsideID, &outsideAuthorType, &outsideContent, &outsideCreatedAt)
		if err == nil {
			latestHuman = &humanCommentData{
				ID:         uuidToString(outsideID),
				AuthorType: outsideAuthorType,
				CreatedAt:  timestampToString(outsideCreatedAt),
				Content:    outsideContent,
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("jev squad routing: failed to query latest human comment", "error", err)
			return nil, fmt.Sprintf("Failed to query latest human comment: %v", err)
		}
	}

	if latestHuman != nil {
		if jev.EstimateTokens(latestHuman.Content) > 14000 {
			return nil, fmt.Sprintf("Latest human comment exceeds comment budget (%d tokens)", jev.EstimateTokens(latestHuman.Content))
		}
		// Opus Blocker 1: Put latest human comment in its own PriorityRequired section
		// so StateBuilder never evicts it under budget pressure.
		stateBuilder.Add("latest_human_comment", map[string]any{
			"id":          latestHuman.ID,
			"author_type": latestHuman.AuthorType,
			"created_at":  latestHuman.CreatedAt,
			"content":     latestHuman.Content,
		}, jev.PriorityRequired)
	}

	if len(eligible) > 0 {
		commentTokens := 0
		var includedIndices []int

		if latestHumanIdx >= 0 {
			commentTokens += jev.EstimateTokens(eligible[latestHumanIdx].Content)
			includedIndices = append(includedIndices, latestHumanIdx)
		}

		// Iterate newest to oldest
		for i := len(eligible) - 1; i >= 0; i-- {
			if i == latestHumanIdx {
				continue
			}
			content := eligible[i].Content
			if jev.EstimateTokens(content) > 1000 {
				content = truncateRunes(content, 3000)
			}
			est := jev.EstimateTokens(content)
			if commentTokens+est > 14000 {
				break
			}
			commentTokens += est
			includedIndices = append(includedIndices, i)
		}

		// Restore chronological order
		sort.Ints(includedIndices)

		formattedComments := make([]map[string]any, 0, len(includedIndices))
		for _, idx := range includedIndices {
			c := eligible[idx]
			content := c.Content
			if idx != latestHumanIdx && jev.EstimateTokens(content) > 1000 {
				content = truncateRunes(content, 3000)
			}
			formattedComments = append(formattedComments, map[string]any{
				"id":          uuidToString(c.ID),
				"author_type": c.AuthorType,
				"created_at":  timestampToString(c.CreatedAt),
				"content":     content,
			})
		}
		stateBuilder.Add("comments", formattedComments, jev.PriorityUseful)
	}

	state, err := stateBuilder.Build()
	if err != nil {
		slog.Warn("jev squad routing: state build failed", "error", err)
		return nil, fmt.Sprintf("State exceeded token budget: %v", err)
	}
	for _, d := range state.Dropped {
		if d == "latest_human_comment" || d == "roster" || d == "squad" || d == "issue" {
			return nil, fmt.Sprintf("Mandatory section %s was dropped", d)
		}
	}

	questionID := "route_squad_member"
	options := make(map[string]string, len(candidates)+1)
	for _, cand := range candidates {
		cid := uuidToString(cand.ID)
		options[cid] = fmt.Sprintf("Agent: %s, Tier: %s, Skills: %s", cand.Name, tiers[cid], strings.Join(skillsByAgentID[cid], ", "))
	}
	options[jevEscalateOption] = "Escalate to squad leader for manual triage or ambiguous task"

	instructions := fmt.Sprintf("Select the most suitable squad member candidate to execute this issue for squad %q based on member specialization, tier rankings (S is highest priority specialist, then A, B, C, D), and issue requirements. If ambiguous, requires human judgment, or no member is suitable, select escalate_to_leader.", squad.Name)

	question := jev.NewChoice(instructions, options)
	req := jev.Request{
		State:     state.Sections,
		Questions: map[string]jev.Question{questionID: question},
	}
	// The client fills an empty model before sending, so hash the request with
	// that model already set: the digest then identifies what Jev receives.
	req.Model = jevClient.Model()
	var telemetry jevTelemetry
	if reqJSON, err := json.Marshal(req); err == nil {
		sum := sha256.Sum256(reqJSON)
		telemetry.inputSHA256 = hex.EncodeToString(sum[:])
	}
	evalCtx, evalCancel := context.WithTimeout(ctx, 5*time.Second)
	defer evalCancel()
	resp, err := jevClient.Evaluate(evalCtx, req)
	if err != nil {
		slog.Warn("jev squad routing: evaluation error", "error", err)
		return &jevDecision{telemetry: telemetry}, fmt.Sprintf("Jev evaluation failed (%v)", err)
	}
	telemetry.servingModel = resp.Model
	telemetry.inputTokens = resp.Usage.InputTokens
	telemetry.outputTokens = resp.Usage.OutputTokens

	answer, ok := resp.Answers[questionID]
	if !ok {
		return &jevDecision{telemetry: telemetry}, "Missing Jev answer for routing"
	}

	offered := make([]string, 0, len(options))
	for k := range options {
		offered = append(offered, k)
	}
	sort.Strings(offered)

	winnerOption, confidence, err := answer.AsStrictChoice(offered)
	if err != nil {
		slog.Warn("jev squad routing: strict choice error", "error", err)
		return &jevDecision{telemetry: telemetry}, fmt.Sprintf("Malformed Jev choice answer: %v", err)
	}

	var runnerUpOption string
	var runnerUpProb float64
	for opt, prob := range answer.Probabilities {
		if opt != winnerOption && prob > runnerUpProb {
			runnerUpOption = opt
			runnerUpProb = prob
		}
	}

	winnerLabel := formatCandidateLabel(winnerOption, candidateMap, tiers)
	runnerUpLabel := formatCandidateLabel(runnerUpOption, candidateMap, tiers)

	return &jevDecision{
		winnerOption:   winnerOption,
		confidence:     confidence,
		runnerUpOption: runnerUpOption,
		runnerUpProb:   runnerUpProb,
		winnerLabel:    winnerLabel,
		runnerUpLabel:  runnerUpLabel,
		candidateMap:   candidateMap,
		tiers:          tiers,
		telemetry:      telemetry,
	}, ""
}

// truncateRunes truncates a string to maxRunes, avoiding splitting UTF-8 characters.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "\n...[truncated]"
}

// parseSquadTierList parses S/A/B/C/D tier rankings from squad instructions.
// It checks markdown tables and bullet list mentions, defaulting unmentioned members to Tier "C".
func parseSquadTierList(instructions string, candidates []db.Agent) map[string]string {
	tiers := make(map[string]string, len(candidates))
	for _, cand := range candidates {
		tiers[uuidToString(cand.ID)] = "C"
	}
	if strings.TrimSpace(instructions) == "" {
		return tiers
	}

	lines := strings.Split(instructions, "\n")
	tierRegex := regexp.MustCompile(`(?:Tier\s*([SABCD])\b|([SABCD])\s*Tier\b)`)

	tableTierCol := -1
	tableMemberCol := -1

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "|") {
			rawCells := strings.Split(trimmed, "|")
			var cells []string
			for _, c := range rawCells {
				cells = append(cells, strings.TrimSpace(c))
			}
			if len(cells) > 2 && cells[0] == "" && cells[len(cells)-1] == "" {
				cells = cells[1 : len(cells)-1]
			}

			// Skip markdown table separator rows like |---|---|
			if strings.Trim(strings.ReplaceAll(trimmed, "|", ""), "-: ") == "" {
				continue
			}

			// If not yet in a table, check for table header row
			if tableTierCol < 0 {
				var tierIdx, memberIdx = -1, -1
				for idx, c := range cells {
					cl := strings.ToLower(c)
					if cl == "tier" || cl == "rank" || cl == "ranking" || cl == "tier ranking" {
						tierIdx = idx
					} else if cl == "member" || cl == "agent" || cl == "specialist" || cl == "name" {
						memberIdx = idx
					}
				}
				if tierIdx >= 0 && memberIdx >= 0 {
					tableTierCol = tierIdx
					tableMemberCol = memberIdx
					continue
				}
			}

			// Table data row
			if tableTierCol >= 0 && tableMemberCol >= 0 && len(cells) > tableTierCol && len(cells) > tableMemberCol {
				memberCell := cells[tableMemberCol]
				tierCell := cells[tableTierCol]
				for _, cand := range candidates {
					candID := uuidToString(cand.ID)
					namePat := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(cand.Name) + `\b`)
					if namePat.MatchString(memberCell) || strings.Contains(memberCell, candID) {
						switch tierCell {
						case "S", "A", "B", "C", "D":
							tiers[candID] = tierCell
						default:
							if m := tierRegex.FindStringSubmatch(tierCell); len(m) > 0 {
								tier := m[1]
								if tier == "" {
									tier = m[2]
								}
								tiers[candID] = strings.ToUpper(tier)
							}
						}
					}
				}
				continue
			}
		} else {
			tableTierCol = -1
			tableMemberCol = -1
		}

		// Non-table line or list
		for _, cand := range candidates {
			candID := uuidToString(cand.ID)
			namePat := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(cand.Name) + `\b`)
			loc := namePat.FindStringIndex(line)
			if loc == nil {
				continue
			}
			// Search for tier strictly within this candidate's clause
			sub := line[loc[1]:]
			endIdx := len(sub)
			for _, delim := range []string{",", ";", ".", "\n"} {
				if pos := strings.Index(sub, delim); pos != -1 && pos < endIdx {
					endIdx = pos
				}
			}
			clause := sub[:endIdx]
			if m := tierRegex.FindStringSubmatch(clause); len(m) > 0 {
				tier := m[1]
				if tier == "" {
					tier = m[2]
				}
				tiers[candID] = strings.ToUpper(tier)
			}
		}
	}

	return tiers
}

func formatCandidateLabel(opt string, candidateMap map[string]db.Agent, tiers map[string]string) string {
	if opt == jevEscalateOption {
		return "Squad Leader (escalate)"
	}
	if agent, ok := candidateMap[opt]; ok {
		tier := tiers[opt]
		return fmt.Sprintf("%s (Tier %s)", agent.Name, tier)
	}
	if opt == "" {
		return "none"
	}
	return opt
}
