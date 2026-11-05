package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/agentconfig"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const runBudgetRaiseType = "issue.run_budget.raise"

// ActionPreviewField is one line of what an approval would change.
type ActionPreviewField struct {
	Label    string `json:"label"`
	Current  string `json:"current"`
	Proposed string `json:"proposed"`
}

// ActionPreview is the server's own rendering of an action, never agent text.
type ActionPreview struct {
	Fields []ActionPreviewField `json:"fields"`
	SHA256 string               `json:"sha256"`
}

// DecisionRequestCardAction is the pending action an open request asks to approve.
type DecisionRequestCardAction struct {
	ID            string `json:"id"`
	ActionType    string `json:"action_type"`
	PayloadSHA256 string `json:"payload_sha256"`
	// Preview is absent when the server cannot render the action as the
	// executor would run it now: an unknown type, or a state the executor refuses.
	Preview *ActionPreview `json:"preview,omitempty"`
}

// previewDigest is the digest of a preview's canonical fields.
func previewDigest(raw []byte) string {
	canonical, err := decisionaction.Canonical(raw)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func newActionPreview(fields []ActionPreviewField) (*ActionPreview, error) {
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return &ActionPreview{Fields: fields, SHA256: previewDigest(raw)}, nil
}

// decisionCardAction reads the request's pending action and renders its
// preview from live state. It returns nil when the request has no pending action.
func (handler *Handler) decisionCardAction(ctx context.Context, workspaceID, requestID pgtype.UUID) *DecisionRequestCardAction {
	action, err := handler.Queries.GetDecisionActionByRequest(ctx, db.GetDecisionActionByRequestParams{DecisionRequestID: requestID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && action.State != "pending") {
		return nil
	}
	if err != nil {
		slog.WarnContext(ctx, "decision card action unreadable", "request_id", uuidToString(requestID), "error", err)
		return nil
	}
	card := &DecisionRequestCardAction{ID: uuidToString(action.ID), ActionType: action.ActionType, PayloadSHA256: action.PayloadSha256}
	fields, err := handler.actionPreviewFields(ctx, action)
	if err == nil && len(fields) > 0 {
		card.Preview, err = newActionPreview(fields)
	}
	if err != nil {
		slog.WarnContext(ctx, "decision card preview unavailable", "action_id", card.ID, "action_type", action.ActionType, "error", err)
		card.Preview = nil
	}
	return card
}

// actionPreviewFields renders the values the executor will check, read
// without writing. An unknown type or a state the executor would refuse gives
// no fields, so the client shows "details unavailable".
func (handler *Handler) actionPreviewFields(ctx context.Context, action db.DecisionAction) ([]ActionPreviewField, error) {
	switch action.ActionType {
	case runBudgetRaiseType:
		var params decisionaction.RunBudgetRaiseParams
		if err := json.Unmarshal(action.Params, &params); err != nil {
			return nil, err
		}
		snapshot, err := handler.openBudgetSnapshot(ctx, params.IssueID, params.WindowID)
		if err != nil || snapshot == nil {
			return nil, err
		}
		if params.NewCap <= snapshot.RunsLimit {
			return nil, nil // the executor refuses a raise that does not raise
		}
		delta := int64(params.NewCap - snapshot.RunsLimit)
		return []ActionPreviewField{
			{"Run cap", strconv.Itoa(snapshot.RunsLimit), strconv.Itoa(params.NewCap)},
			{"Runs left", strconv.FormatInt(snapshot.RunsLeft, 10), strconv.FormatInt(snapshot.RunsLeft+delta, 10)},
		}, nil
	case runBudgetTopUpType:
		var params decisionaction.RunBudgetTopUpParams
		if err := json.Unmarshal(action.Params, &params); err != nil {
			return nil, err
		}
		snapshot, err := handler.openBudgetSnapshot(ctx, params.IssueID, params.WindowID)
		if err != nil || snapshot == nil {
			return nil, err
		}
		return []ActionPreviewField{{"Runs used", strconv.FormatInt(snapshot.RunsUsed, 10), "0"}}, nil
	case "agent.env.patch":
		return handler.envPatchPreview(ctx, action)
	case "agent.copy":
		return handler.agentCopyPreview(ctx, action)
	}
	return nil, nil
}

// openBudgetSnapshot reads the issue's run budget as the executor does, and
// returns nil when the issue is unbudgeted or its window is no longer the one
// the action names (the executor ends both with a failure).
func (handler *Handler) openBudgetSnapshot(ctx context.Context, issueID, windowID string) (*service.RunBudgetSnapshot, error) {
	id, err := parseUUIDString(issueID)
	if err != nil {
		return nil, err
	}
	issue, err := handler.Queries.GetIssue(ctx, id)
	if err != nil {
		return nil, err
	}
	snapshot, err := service.LoadRunBudgetSnapshot(ctx, handler.Queries, issue)
	if err != nil || !snapshot.Budgeted || snapshot.WindowID != windowID {
		return nil, err
	}
	return &snapshot, nil
}

// envPatchPreview names the agent and the env keys the patch sets or removes;
// values are secrets and never leave the server.
func (handler *Handler) envPatchPreview(ctx context.Context, action db.DecisionAction) ([]ActionPreviewField, error) {
	var params decisionaction.EnvPatchParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	agent, err := handler.agentForAction(ctx, action.WorkspaceID, params.AgentID)
	if err != nil {
		return nil, err
	}
	existing := unmarshalCustomEnv(agent)
	fields := []ActionPreviewField{{"Agent", agent.Name, agent.Name}}
	setKeys := make([]string, 0, len(params.Set))
	for key := range params.Set {
		setKeys = append(setKeys, key)
	}
	if len(setKeys) > 0 {
		fields = append(fields, ActionPreviewField{"Set keys", keysPresent(existing, setKeys), joinKeys(setKeys)})
	}
	if len(params.Unset) > 0 {
		fields = append(fields, ActionPreviewField{"Unset keys", keysPresent(existing, params.Unset), "removed"})
	}
	return fields, nil
}

// agentCopyPreview lists every setting the copy will run with, next to the
// source's value: the approver signs the whole execution profile, not only the
// name. The copy never carries the source's env or MCP config.
func (handler *Handler) agentCopyPreview(ctx context.Context, action db.DecisionAction) ([]ActionPreviewField, error) {
	var params decisionaction.AgentCopyParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	source, err := handler.agentForAction(ctx, action.WorkspaceID, params.SourceAgentID)
	if err != nil {
		return nil, err
	}
	runtimeID, err := parseUUIDStrict(params.RuntimeID)
	if err != nil {
		return nil, fmt.Errorf("copy target runtime: %w", err)
	}
	target, err := handler.Queries.GetAgentRuntime(ctx, runtimeID)
	if err != nil {
		return nil, err
	}
	current := runtimeLabel{id: source.RuntimeID}
	if source.RuntimeID.Valid {
		if runtime, err := handler.Queries.GetAgentRuntime(ctx, source.RuntimeID); err == nil {
			current = runtimeLabel{id: runtime.ID, name: runtime.Name, provider: runtime.Provider}
		}
	}
	proposedInstructions := source.Instructions
	if params.Instructions != nil {
		proposedInstructions = *params.Instructions
	}
	maxConcurrent := params.MaxConcurrentTasks
	if maxConcurrent == 0 {
		maxConcurrent = agentconfig.DefaultMaxConcurrentTasks
	}
	return []ActionPreviewField{
		{"Agent", source.Name, params.Name},
		{"Runtime", current.String(), runtimeLabel{id: target.ID, name: target.Name, provider: target.Provider}.String()},
		{"Instructions", source.Instructions, proposedInstructions},
		{"Description", source.Description, params.Description},
		{"Model", orDefault(source.Model.String), orDefault(params.Model)},
		{"Thinking level", orDefault(source.ThinkingLevel.String), orDefault(params.ThinkingLevel)},
		{"Service tier", orDefault(source.ServiceTier.String), orDefault(params.ServiceTier)},
		{"Custom args", orNone(strings.Join(unmarshalArgs(source.CustomArgs), " ")), orNone(strings.Join(params.CustomArgs, " "))},
		{"Max concurrent tasks", fmt.Sprint(source.MaxConcurrentTasks), fmt.Sprint(maxConcurrent)},
		{"Conversation starters", startersLabel(source.ConversationStarters), startersParamsLabel(params.ConversationStarters)},
		{"Avatar", source.AvatarUrl.String, avatarLabel(params.AvatarURL)},
	}, nil
}

type runtimeLabel struct {
	id             pgtype.UUID
	name, provider string
}

func (label runtimeLabel) String() string {
	if label.name == "" {
		return uuidToString(label.id)
	}
	return fmt.Sprintf("%s (%s)", label.name, label.provider)
}

func orDefault(value string) string {
	if value == "" {
		return "default"
	}
	return value
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func unmarshalArgs(raw []byte) []string {
	var args []string
	_ = json.Unmarshal(raw, &args)
	return args
}

func startersParamsLabel(starters []decisionaction.ConversationStarter) string {
	parts := make([]string, len(starters))
	for i, starter := range starters {
		parts[i] = starter.Label + ": " + starter.Prompt
	}
	return orNone(strings.Join(parts, "; "))
}

func startersLabel(raw []byte) string {
	var starters []decisionaction.ConversationStarter
	_ = json.Unmarshal(raw, &starters)
	return startersParamsLabel(starters)
}

// avatarLabel: a copy without a named image gets a random emoji.
func avatarLabel(requested *string) string {
	if requested == nil || *requested == "" {
		return "random emoji"
	}
	return *requested
}

func (handler *Handler) agentForAction(ctx context.Context, workspaceID pgtype.UUID, agentID string) (db.Agent, error) {
	id, err := parseUUIDStrict(agentID)
	if err != nil {
		return db.Agent{}, fmt.Errorf("action target agent: %w", err)
	}
	return handler.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: workspaceID})
}

func joinKeys(keys []string) string {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	return strings.Join(sorted, ", ")
}

// keysPresent lists, of keys, those the agent's env already has ("none" if no one).
func keysPresent(existing map[string]string, keys []string) string {
	var present []string
	for _, key := range keys {
		if _, found := existing[key]; found {
			present = append(present, key)
		}
	}
	if len(present) == 0 {
		return "none"
	}
	return joinKeys(present)
}
