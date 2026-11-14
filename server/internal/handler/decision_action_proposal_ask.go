package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// askChangeMaxLines caps the before → after block so a large rewrite never
	// pushes the ask past the comment limit; the card shows the full preview.
	askChangeMaxLines = 40
	askValueMaxRunes  = 120
	askDiffMaxRunes   = 500
	// askProposerLabel stands in for the proposing agent's own name: the
	// server-composed ask never names the agent.
	askProposerLabel = "the proposing agent"
)

// composeProposalAsk is the ask text of a proposal. The server writes it from
// the action's template and preview; the agent's reason appears only as a
// labelled quote. change is the rendered before → after block, empty when the
// preview is unavailable.
func composeProposalAsk(issueID string, draft *decisionActionDraft, change, reason string) (string, error) {
	params, err := json.MarshalIndent(draft.Params, "", "  ")
	if err != nil {
		return "", err
	}
	ask := "**Proposed action**\n"
	if template := draft.Definition.Template; template.Title != "" {
		ask += "**" + template.Title + "**\n" + template.Summary + "\n"
	}
	ask += "`" + draft.Type + "` version " + strconv.Itoa(draft.Version) + " on [Issue](mention://issue/" + issueID + ")\n\n"
	if change != "" {
		ask += "**Before → after**\n" + change + "\n\n"
	}
	ask += "**Parameters**\n```json\n" + string(params) + "\n```"
	if reason = strings.TrimSpace(reason); reason != "" {
		ask += "\n\n**Agent's note (not verified)**\n> " + strings.ReplaceAll(strings.ReplaceAll(reason, "\r", ""), "\n", "\n> ")
	}
	return ask, nil
}

// proposalChange renders the before → after block from the same preview the
// approval signs: a config action's stored diff comes from Read and
// configChangeDiff, any other action's rows from actionPreviewFields. A preview
// that cannot be read gives "", and the transaction judges the proposal itself.
func (handler *Handler) proposalChange(ctx context.Context, issue db.Issue, agentID, recipient pgtype.UUID, draft *decisionActionDraft) string {
	if change, ok := configChanges[draft.Type]; ok {
		read, err := change.Read(ctx, handler.Queries, issue.WorkspaceID, draft.Params)
		if err != nil {
			return ""
		}
		if strings.HasPrefix(read.Field, "agent.") { // the card withholds the same preview
			if _, err := handler.agentForAction(ctx, issue.WorkspaceID, read.TargetID, recipient); err != nil {
				return ""
			}
		}
		_, hunks, err := configChangeDiff(read)
		if err != nil {
			return ""
		}
		return renderDiffBlock(hunks)
	}
	rows, err := handler.actionPreviewFields(ctx, db.DecisionAction{WorkspaceID: issue.WorkspaceID, ActionType: draft.Type, Params: draft.Params}, recipient)
	if err != nil {
		return ""
	}
	proposer := ""
	if agent, err := handler.Queries.GetAgent(ctx, agentID); err == nil {
		proposer = agent.Name
	}
	return renderRowsBlock(rows, proposer)
}

func renderDiffBlock(hunks []decisiontx.DiffHunk) string {
	lines, omitted := []string{}, 0
	for _, hunk := range hunks {
		block := []string{fmt.Sprintf("@@ -%d +%d @@", hunk.OldStart, hunk.NewStart)}
		for _, line := range hunk.Lines {
			if runes := []rune(line); len(runes) > askDiffMaxRunes {
				line = string(runes[:askDiffMaxRunes]) + "…"
			}
			block = append(block, line)
		}
		if room := askChangeMaxLines - len(lines); len(block) > room {
			omitted += len(block) - max(room, 0)
			block = block[:max(room, 0)]
		}
		lines = append(lines, block...)
	}
	if len(lines) == 0 {
		return ""
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("… %d more diff lines; the card shows the full change", omitted))
	}
	return fencedBlock("diff", lines)
}

// renderRowsBlock lists the rows whose value changes, one line each. Values
// are cut to one short line, and the proposer's own name is replaced.
func renderRowsBlock(rows []ActionPreviewField, proposer string) string {
	lines := []string{}
	for _, row := range rows {
		if row.Current == row.Proposed {
			continue
		}
		lines = append(lines, row.Label+": "+askValue(row.Current, proposer)+" → "+askValue(row.Proposed, proposer))
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > askChangeMaxLines {
		lines = append(lines[:askChangeMaxLines], fmt.Sprintf("… %d more rows; the card shows every row", len(lines)-askChangeMaxLines))
	}
	return fencedBlock("text", lines)
}

func askValue(value, proposer string) string {
	if proposer != "" && value == proposer {
		return askProposerLabel
	}
	value = strings.Join(strings.Fields(value), " ")
	if runes := []rune(value); len(runes) > askValueMaxRunes {
		value = string(runes[:askValueMaxRunes]) + "…"
	}
	return value
}

func fencedBlock(language string, lines []string) string {
	body := strings.Join(lines, "\n")
	fence := decisiontx.FenceFor(body)
	return fence + language + "\n" + body + "\n" + fence
}
