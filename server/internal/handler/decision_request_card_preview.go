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
	"github.com/multica-ai/multica/server/internal/decisiontx"
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
	// OwnerOnly: only the workspace owner may approve (the server refuses
	// anyone else with needs_owner); the card hides the approve button.
	OwnerOnly bool `json:"owner_only,omitempty"`
	// ConfigChange is the stored server preview of a pending config change,
	// the exact text whose digest approve checks. Absent for other actions.
	ConfigChange *ConfigChangeCard `json:"config_change,omitempty"`
}

// ConfigChangeCard is a config change's stored preview and its digest.
type ConfigChangeCard struct {
	Field    string                `json:"field"`
	TargetID string                `json:"target_id"`
	Keys     []string              `json:"keys"`
	DiffKind string                `json:"diff_kind"`
	Diff     []decisiontx.DiffHunk `json:"diff"`
	// After is the value approve would write, as the server read it: a string
	// for text, a list of lines, or a JSON value.
	After        any            `json:"after"`
	BeforeSHA256 string         `json:"before_sha256"`
	AfterSHA256  string         `json:"after_sha256"`
	Meta         map[string]any `json:"meta,omitempty"`
	SHA256       string         `json:"sha256"`
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
	card := &DecisionRequestCardAction{ID: uuidToString(action.ID), ActionType: action.ActionType, PayloadSHA256: action.PayloadSha256, State: action.State, OwnerOnly: actionOwnerOnly(action)}
	if action.State != "pending" {
		return card
	}
	if _, configured := configChanges[action.ActionType]; configured {
		card.ConfigChange = handler.cardConfigChange(ctx, action)
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
	return card
}

// cardConfigChange reads the preview stored with the action, the one approve
// checks by digest. It quotes agent settings, so it is withheld for an agent
// the card's human could not view. Any unreadable part gives no preview, and the
// client then offers no approve.
func (handler *Handler) cardConfigChange(ctx context.Context, action db.DecisionAction) *ConfigChangeCard {
	var stored struct {
		Preview       ConfigChangeCard `json:"preview"`
		PreviewSHA256 string           `json:"preview_sha256"`
	}
	if err := json.Unmarshal(action.Preconditions, &stored); err != nil || stored.PreviewSHA256 == "" || stored.Preview.Field == "" {
		slog.WarnContext(ctx, "decision card config change unreadable", "action_id", uuidToString(action.ID), "error", err)
		return nil
	}
	if strings.HasPrefix(stored.Preview.Field, "agent.") {
		request, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: action.WorkspaceID, ID: action.DecisionRequestID})
		if err == nil {
			_, err = handler.agentForAction(ctx, action.WorkspaceID, stored.Preview.TargetID, request.HumanUserID)
		}
		if err != nil {
			slog.WarnContext(ctx, "decision card config change withheld", "action_id", uuidToString(action.ID), "error", err)
			return nil
		}
	}
	card := stored.Preview
	card.SHA256 = stored.PreviewSHA256
	if card.Keys == nil {
		card.Keys = []string{}
	}
	// A preview stored before diff_kind existed is the json kind.
	if card.DiffKind == "" {
		card.DiffKind = "json"
	}
	if card.Diff == nil {
		card.Diff = []decisiontx.DiffHunk{}
	}
	return &card
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
	case issueProjectSetType:
		return handler.issueProjectSetPreview(ctx, action)
	case "agent.env.patch":
		return handler.envPatchPreview(ctx, action, viewer)
	case "agent.copy":
		return handler.agentCopyPreview(ctx, action, viewer)
	case agentPauseAction, agentResumeAction:
		return handler.agentSchedulingPreview(ctx, action, viewer)
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
	target, err := handler.runtimeForAction(ctx, action.WorkspaceID, runtimeID, viewer)
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

// runtimeForAction reads the target runtime as the executor accepts it: in the
// card's workspace and usable by the viewer. Anything else is an error, so the
// card shows "details unavailable" and names nothing about the runtime.
func (handler *Handler) runtimeForAction(ctx context.Context, workspaceID, runtimeID, viewer pgtype.UUID) (db.AgentRuntime, error) {
	runtime, err := handler.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{ID: runtimeID, WorkspaceID: workspaceID})
	if err != nil {
		return db.AgentRuntime{}, err
	}
	member, err := handler.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: viewer, WorkspaceID: workspaceID})
	if err != nil {
		return db.AgentRuntime{}, err
	}
	if !canUseRuntimeForAgent(member, runtime) {
		return db.AgentRuntime{}, errors.New("the card's recipient cannot use the action's runtime")
	}
	return runtime, nil
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
