package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/jev"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// tryJevSquadRouting attempts server-side fast-path routing using TypeSafe AI's Jev model.
// When confidence >= minConfidence (default 0.90) and snapshot freshness validates atomically,
// it dispatches the task directly to the selected squad worker, updates status (backlog -> in_progress),
// writes an announcement comment (type='jev_route'), and records squad activity.
// On confidence < minConfidence or error, it returns (false, advisory) where advisory contains
// pre-computed recommendations to append to the squad leader's handoff note.
func (h *Handler) tryJevSquadRouting(ctx context.Context, issue db.Issue, squad db.Squad, authorType, authorID string) (bool, string) {
	config, err := loadGovernanceControl(ctx, h.DB, issue.WorkspaceID)
	if err != nil || !config.Settings.JevSquadRoutingEnabled {
		return false, ""
	}

	decisionID := fmt.Sprintf("jd_%x", time.Now().UnixNano()&0xffff)
	apiURL := config.Settings.JevAPIURL
	minConfidence := 0.90
	if config.Settings.JevMinConfidence != nil {
		minConfidence = *config.Settings.JevMinConfidence
	}

	var jevClient *jev.Client
	if apiKeyBytes, err := h.ResolveJevCredential(ctx, issue.WorkspaceID); err == nil && len(apiKeyBytes) > 0 {
		opts := jev.Options{
			APIKey:  string(apiKeyBytes),
			BaseURL: apiURL,
		}
		jevClient, _ = jev.NewClient(opts)
	} else if h.Jev != nil {
		jevClient = h.Jev
	}

	if jevClient == nil {
		slog.Warn("jev squad routing: credentials unconfigured", "workspace_id", uuidToString(issue.WorkspaceID))
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: none\n- Runner-up: none\n- Ambiguity: Jev provider credential unavailable\n- Prior State Changes: none\n- Decision ID: %s", decisionID)
		return false, advisory
	}

	members, err := h.Queries.ListSquadMembers(ctx, squad.ID)
	if err != nil {
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: none\n- Runner-up: none\n- Ambiguity: Failed to load squad members: %v\n- Prior State Changes: none\n- Decision ID: %s", err, decisionID)
		return false, advisory
	}

	candidates := make([]db.Agent, 0, len(members))
	candidateMap := make(map[string]db.Agent, len(members))
	for _, m := range members {
		if m.MemberType != "agent" || uuidToString(m.MemberID) == uuidToString(squad.LeaderID) {
			continue
		}
		agent, err := h.Queries.GetAgent(ctx, m.MemberID)
		if err != nil || agent.ArchivedAt.Valid || !agent.RuntimeID.Valid {
			continue
		}
		candidates = append(candidates, agent)
		candidateMap[uuidToString(agent.ID)] = agent
	}

	if len(candidates) == 0 {
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: none\n- Runner-up: none\n- Ambiguity: No eligible active squad member agents with valid runtimes\n- Prior State Changes: none\n- Decision ID: %s", decisionID)
		return false, advisory
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
		maxLen := 12000
		if len(desc) < maxLen {
			maxLen = len(desc)
		}
		desc = desc[:maxLen] + "\n...[truncated]"
	}
	stateBuilder.Add("issue", map[string]any{
		"id":          uuidToString(issue.ID),
		"title":       issue.Title,
		"description": desc,
		"status":      issue.Status,
		"priority":    issue.Priority,
		"revision":    issue.Revision,
	}, jev.PriorityRequired)

	if comments, err := h.Queries.ListCommentsForIssue(ctx, db.ListCommentsForIssueParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		Limit:       20,
	}); err == nil && len(comments) > 0 {
		formattedComments := make([]map[string]any, 0, len(comments))
		for _, c := range comments {
			if c.Type == "jev_route" || strings.TrimSpace(c.Content) == "" {
				continue
			}
			content := c.Content
			if jev.EstimateTokens(content) > 1000 {
				maxLen := 3000
				if len(content) < maxLen {
					maxLen = len(content)
				}
				content = content[:maxLen] + "...[truncated]"
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
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: none\n- Runner-up: none\n- Ambiguity: State exceeded token budget: %v\n- Prior State Changes: none\n- Decision ID: %s", err, decisionID)
		return false, advisory
	}

	questionID := "route_squad_member"
	options := make(map[string]string, len(candidates)+1)
	for _, cand := range candidates {
		cid := uuidToString(cand.ID)
		options[cid] = fmt.Sprintf("Agent: %s, Tier: %s, Skills: %s", cand.Name, tiers[cid], strings.Join(skillsByAgentID[cid], ", "))
	}
	options["escalate_to_leader"] = "Escalate to squad leader for manual triage or ambiguous task"

	instructions := fmt.Sprintf("Select the most suitable squad member candidate to execute this issue for squad %q based on member specialization, tier rankings (S is highest priority specialist, then A, B, C, D), and issue requirements. If ambiguous, requires human judgment, or no member is suitable, select escalate_to_leader.", squad.Name)

	question := jev.NewChoice(instructions, options)
	req := jev.Request{
		State:     state.Sections,
		Questions: map[string]jev.Question{questionID: question},
	}
	resp, err := jevClient.Evaluate(ctx, req)
	if err != nil {
		slog.Warn("jev squad routing: evaluation error", "error", err)
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: none\n- Runner-up: none\n- Ambiguity: Jev evaluation failed (%v)\n- Prior State Changes: none\n- Decision ID: %s", err, decisionID)
		return false, advisory
	}

	answer, ok := resp.Answers[questionID]
	if !ok {
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: none\n- Runner-up: none\n- Ambiguity: Missing Jev answer for routing\n- Prior State Changes: none\n- Decision ID: %s", decisionID)
		return false, advisory
	}

	offered := make([]string, 0, len(options))
	for k := range options {
		offered = append(offered, k)
	}
	sort.Strings(offered)

	winnerOption, confidence, err := answer.AsStrictChoice(offered)
	if err != nil {
		slog.Warn("jev squad routing: strict choice error", "error", err)
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: none\n- Runner-up: none\n- Ambiguity: Malformed Jev choice answer: %v\n- Prior State Changes: none\n- Decision ID: %s", err, decisionID)
		return false, advisory
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

	if winnerOption == "escalate_to_leader" || confidence < minConfidence {
		ambiguity := fmt.Sprintf("Confidence %.2f is below %.2f threshold for automated dispatch", confidence, minConfidence)
		if winnerOption == "escalate_to_leader" {
			ambiguity = "Jev recommended escalation to squad leader"
		}
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: %s · Conf %.2f\n- Runner-up: %s · Conf %.2f\n- Ambiguity: %s\n- Prior State Changes: none\n- Decision ID: %s",
			winnerLabel, confidence, runnerUpLabel, runnerUpProb, ambiguity, decisionID)
		return false, advisory
	}

	winnerAgent := candidateMap[winnerOption]
	winnerTier := tiers[winnerOption]

	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		slog.Error("jev squad routing: tx begin error", "error", err)
		advisory := fmt.Sprintf("### Jev Pre-Route Advisory\n- Proposed: %s · Conf %.2f\n- Runner-up: %s · Conf %.2f\n- Ambiguity: Database transaction failed: %v\n- Prior State Changes: none\n- Decision ID: %s", winnerLabel, confidence, runnerUpLabel, runnerUpProb, err, decisionID)
		return false, advisory
	}
	defer tx.Rollback(ctx)

	var currentRevision int64
	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT revision, status FROM issue WHERE id = $1 FOR UPDATE`, issue.ID).Scan(&currentRevision, &currentStatus)
	if err != nil {
		return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Failed to lock issue: %v\n- Decision ID: %s", err, decisionID)
	}

	if currentRevision != issue.Revision {
		slog.Warn("jev squad routing: revision conflict", "expected", issue.Revision, "actual", currentRevision)
		return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Issue revision changed during evaluation (expected %d, found %d)\n- Prior State Changes: none\n- Decision ID: %s", issue.Revision, currentRevision, decisionID)
	}

	statusChanged := false
	if currentStatus == "backlog" {
		_, err = tx.Exec(ctx, `UPDATE issue SET status = 'in_progress', updated_at = now(), revision = revision + 1 WHERE id = $1`, issue.ID)
		if err != nil {
			return false, fmt.Sprintf("### Jev Pre-Route Advisory\n- Ambiguity: Failed to update issue status: %v\n- Decision ID: %s", err, decisionID)
		}
		statusChanged = true
	}

	commentID := dbid.NewV7()
	commentContent := fmt.Sprintf("⚡ Jev routed to %s (Tier %s) · Conf %.2f · Reason: High-confidence specialist match · Decision %s", winnerAgent.Name, winnerTier, confidence, decisionID)

	qtx := h.Queries.WithTx(tx)
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
		ActorID:     winnerAgent.ID,
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
		issue.Status = "in_progress"
		issue.Revision = currentRevision + 1
		h.publish(protocol.EventIssueUpdated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
			"issue_id": uuidToString(issue.ID),
			"status":   "in_progress",
		})
	}

	commentEvent := map[string]any{
		"comment": map[string]any{
			"id":          uuidToString(commentRow.ID),
			"issue_id":    uuidToString(issue.ID),
			"author_type": "system",
			"author_id":   nil,
			"content":     commentContent,
			"type":        "jev_route",
			"created_at":  timestampToString(commentRow.CreatedAt),
		},
	}
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", commentEvent)

	h.publish(protocol.EventActivityCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"issue_id": uuidToString(issue.ID),
		"entry": map[string]any{
			"type":       "activity",
			"id":         uuidToString(activity.ID),
			"actor_type": "system",
			"actor_id":   uuidToString(winnerAgent.ID),
			"action":     activity.Action,
			"details":    json.RawMessage(detailsJSON),
			"created_at": timestampToString(activity.CreatedAt),
		},
	})

	if _, err = h.TaskService.EnqueueTaskForMention(ctx, issue, winnerAgent.ID, pgtype.UUID{}, service.OriginDerived); err != nil {
		slog.Warn("jev dispatch: failed to enqueue member task",
			"issue_id", uuidToString(issue.ID),
			"agent_id", uuidToString(winnerAgent.ID),
			"error", err)
	}

	return true, ""
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
	tierRegex := regexp.MustCompile(`(?i)\b(?:Tier\s*([SABCD])\b|([SABCD])\s*Tier\b)`)

	for _, cand := range candidates {
		candID := uuidToString(cand.ID)
		candName := strings.ToLower(cand.Name)
		aliases := []string{candName, candID}
		parts := strings.Split(candName, "-")
		if len(parts) > 1 {
			aliases = append(aliases, parts[len(parts)-1])
			if len(parts) > 2 {
				aliases = append(aliases, parts[len(parts)-2]+"-"+parts[len(parts)-1])
			}
		}

	LineLoop:
		for _, line := range lines {
			lineLower := strings.ToLower(line)
			matched := false
			for _, alias := range aliases {
				if strings.Contains(lineLower, alias) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}

			if strings.Contains(line, "|") {
				cells := strings.Split(line, "|")
				for _, cell := range cells {
					trimmed := strings.TrimSpace(cell)
					if len(trimmed) == 1 && strings.ContainsAny(strings.ToUpper(trimmed), "SABCD") {
						tiers[candID] = strings.ToUpper(trimmed)
						break LineLoop
					}
					if m := tierRegex.FindStringSubmatch(cell); len(m) > 0 {
						tier := m[1]
						if tier == "" {
							tier = m[2]
						}
						tiers[candID] = strings.ToUpper(tier)
						break LineLoop
					}
				}
			}

			if m := tierRegex.FindStringSubmatch(line); len(m) > 0 {
				tier := m[1]
				if tier == "" {
					tier = m[2]
				}
				tiers[candID] = strings.ToUpper(tier)
				break LineLoop
			}
		}
	}

	return tiers
}

func formatCandidateLabel(opt string, candidateMap map[string]db.Agent, tiers map[string]string) string {
	if opt == "escalate_to_leader" {
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
