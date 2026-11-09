package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/agentconfig"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
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

// ActionDiff is the stored text diff an approver must be shown before a
// RequiresPreview action; SHA256 is the digest approval compares.
type ActionDiff struct {
	Kind   string                `json:"kind"`
	Hunks  []decisiontx.DiffHunk `json:"hunks"`
	SHA256 string                `json:"sha256"`
	// Target is the workflow step the diff applies to, resolved by the server
	// from the live workflow, never from the agent's ask text.
	Target *ActionDiffTarget `json:"target,omitempty"`
}

// ActionDiffTarget names the workflow and status key an approval would change.
type ActionDiffTarget struct {
	WorkflowID   string `json:"workflow_id"`
	WorkflowName string `json:"workflow_name"`
	StatusKey    string `json:"status_key"`
}

// DecisionRequestCardAction is the action a request asks to approve. It stays on
// the card after approval so the card can show the outcome.
type DecisionRequestCardAction struct {
	ID            string `json:"id"`
	ActionType    string `json:"action_type"`
	PayloadSHA256 string `json:"payload_sha256"`
	State         string `json:"state"`
	// Preview is absent when the server cannot render the action as the
	// executor would run it now: an unknown type, or a state the executor refuses.
	Preview *ActionPreview `json:"preview,omitempty"`
	// Diff is the stored change of a config-change action, pending only.
	Diff *ActionDiff `json:"diff,omitempty"`
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

// decisionCardAction reads the request's action. A pending action gets a
// preview rendered from live state; any other state carries none. It returns nil
// when the request has no action.
func (handler *Handler) decisionCardAction(ctx context.Context, workspaceID, requestID pgtype.UUID) *DecisionRequestCardAction {
	action, err := handler.Queries.GetDecisionActionByRequest(ctx, db.GetDecisionActionByRequestParams{DecisionRequestID: requestID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		slog.WarnContext(ctx, "decision card action unreadable", "request_id", uuidToString(requestID), "error", err)
		return nil
	}
	card := &DecisionRequestCardAction{ID: uuidToString(action.ID), ActionType: action.ActionType, PayloadSHA256: action.PayloadSha256, State: action.State}
	if action.State != "pending" {
		return card
	}
	fields, err := handler.cardPreviewFields(ctx, action)
	if err == nil && len(fields) > 0 {
		card.Preview, err = newActionPreview(fields)
	}
	if err != nil {
		slog.WarnContext(ctx, "decision card preview unavailable", "action_id", card.ID, "action_type", action.ActionType, "error", err)
		card.Preview = nil
	}
	card.Diff = storedActionDiff(action.Preconditions)
	if card.Diff != nil && action.ActionType == decisionaction.WorkflowStepInstructionsSet {
		// The diff alone does not say which step it edits, so the card names the
		// target from live state. A target that cannot be resolved withholds the
		// diff and with it the approve button.
		card.Diff.Target = handler.stepInstructionsDiffTarget(ctx, action)
		if card.Diff.Target == nil {
			card.Diff = nil
		}
	}
	return card
}

// stepInstructionsDiffTarget resolves the workflow and status key a pending
// workflow.step.instructions.set action would change, from the live workflow
// and the action's validated params, never from the request's ask text. It is
// nil when the workflow or its step is gone.
func (handler *Handler) stepInstructionsDiffTarget(ctx context.Context, action db.DecisionAction) *ActionDiffTarget {
	var wanted decisionaction.WorkflowStepInstructionsSetParams
	if json.Unmarshal(action.Params, &wanted) != nil {
		return nil
	}
	workflow, err := loadWorkflowTarget(ctx, handler.Queries, action.WorkspaceID, action.Params, http.StatusConflict, false)
	if err != nil {
		return nil
	}
	steps, err := issueworkflow.DecodeSteps(workflow.Steps)
	if err != nil || !slices.ContainsFunc(steps, func(step issueworkflow.Step) bool { return step.StatusKey == wanted.StatusKey }) {
		return nil
	}
	return &ActionDiffTarget{WorkflowID: uuidToString(workflow.ID), WorkflowName: workflow.Name, StatusKey: wanted.StatusKey}
}

// storedActionDiff reads the text diff a config-change action stored with its
// preview, and the digest approval compares. It is nil for an action without
// one or whose stored preview is not a text diff.
func storedActionDiff(preconditions []byte) *ActionDiff {
	var stored struct {
		Preview struct {
			Diff     []decisiontx.DiffHunk `json:"diff"`
			DiffKind string                `json:"diff_kind"`
		} `json:"preview"`
		SHA256 string `json:"preview_sha256"`
	}
	if json.Unmarshal(preconditions, &stored) != nil || stored.Preview.DiffKind != diffKindText || stored.SHA256 == "" {
		return nil
	}
	return &ActionDiff{Kind: diffKindText, Hunks: stored.Preview.Diff, SHA256: stored.SHA256}
}

// cardPreviewFields renders the preview for the request's human, the only
// reader of the card. A preview names or quotes agent settings, so it is
// withheld for an agent that human could not view: the executor refuses the
// same case at approval, after the disclosure.
func (handler *Handler) cardPreviewFields(ctx context.Context, action db.DecisionAction) ([]ActionPreviewField, error) {
	request, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: action.WorkspaceID, ID: action.DecisionRequestID})
	if err != nil {
		return nil, err
	}
	return handler.actionPreviewFields(ctx, action, request.HumanUserID)
}

// actionPreviewFields renders the values the executor will check, read
// without writing. An unknown type or a state the executor would refuse gives
// no fields, so the client shows "details unavailable".
func (handler *Handler) actionPreviewFields(ctx context.Context, action db.DecisionAction, viewer pgtype.UUID) ([]ActionPreviewField, error) {
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
		return handler.envPatchPreview(ctx, action, viewer)
	case "agent.copy":
		return handler.agentCopyPreview(ctx, action, viewer)
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
func (handler *Handler) envPatchPreview(ctx context.Context, action db.DecisionAction, viewer pgtype.UUID) ([]ActionPreviewField, error) {
	var params decisionaction.EnvPatchParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	agent, err := handler.agentForAction(ctx, action.WorkspaceID, params.AgentID, viewer)
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
func (handler *Handler) agentCopyPreview(ctx context.Context, action db.DecisionAction, viewer pgtype.UUID) ([]ActionPreviewField, error) {
	var params decisionaction.AgentCopyParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	source, err := handler.agentForAction(ctx, action.WorkspaceID, params.SourceAgentID, viewer)
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
	targets, err := handler.Queries.ListAgentInvocationTargets(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	skills, err := handler.Queries.ListAgentSkills(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	access, skillNames := inheritedAccess(source, targets), inheritedSkills(skills)
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
		{"Who can invoke", access, access},
		{"Skills", skillNames, skillNames},
	}, nil
}

// inheritedAccess and inheritedSkills show what a copy takes over unchanged
// from the source: permission mode, invocation targets and skills.
func inheritedAccess(source db.Agent, targets []db.AgentInvocationTarget) string {
	labels := make([]string, len(targets))
	for i, target := range targets {
		labels[i] = target.TargetType + ":" + uuidToString(target.TargetID)
	}
	sort.Strings(labels)
	return source.PermissionMode + ": " + orNone(strings.Join(labels, ", "))
}

func inheritedSkills(skills []db.Skill) string {
	names := make([]string, len(skills))
	for i, skill := range skills {
		names[i] = skill.Name
	}
	sort.Strings(names)
	return orNone(strings.Join(names, ", "))
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

func (handler *Handler) agentForAction(ctx context.Context, workspaceID pgtype.UUID, agentID string, viewer pgtype.UUID) (db.Agent, error) {
	id, err := parseUUIDStrict(agentID)
	if err != nil {
		return db.Agent{}, fmt.Errorf("action target agent: %w", err)
	}
	agent, err := handler.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: workspaceID})
	if err != nil {
		return db.Agent{}, err
	}
	member, err := handler.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: viewer, WorkspaceID: workspaceID})
	if err != nil {
		return db.Agent{}, err
	}
	targets, err := handler.Queries.ListAgentInvocationTargets(ctx, agent.ID)
	if err != nil {
		return db.Agent{}, err
	}
	if !memberAllowedToViewAgent(agent, targets, uuidToString(viewer), member.Role) {
		return db.Agent{}, errors.New("the card's recipient cannot view the action's agent")
	}
	return agent, nil
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
