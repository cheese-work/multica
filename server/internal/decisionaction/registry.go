package decisionaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// Error codes the request endpoint maps to HTTP 400.
const (
	CodeUnknownAction  = "unknown_action"
	CodeInvalidParams  = "invalid_params"
	CodeSecretInParams = "secret_in_params"
)

// Error carries a stable code; its message never echoes a param value.
type Error struct{ Code, Message string }

func (e *Error) Error() string { return e.Message }

func invalid(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// Request is what an agent submits.
type Request struct {
	Type        string
	Version     int
	Params      json.RawMessage
	SecretSlots []SecretSlot
}

// Normalized is the request in its one canonical shape: the same intent always
// yields the same bytes, so the payload hash does not depend on spelling.
type Normalized struct {
	Definition  *Definition
	Params      json.RawMessage
	SecretSlots []SecretSlot
}

// Target is the kind of row an action reads or changes. The zero value is an
// agent.
type Target string

const (
	TargetAgent Target = ""
	TargetIssue Target = "issue"
	// TargetWorkflow is a workflow of the request's workspace, named by
	// params.workflow_id.
	TargetWorkflow Target = "workflow"
	// TargetSquad is a squad of the request's workspace, named by params.squad_id.
	TargetSquad Target = "squad"
	// TargetWorkspace is the request's own workspace, named by params.workspace_id.
	TargetWorkspace Target = "workspace"
)

// Definition is one allowlisted action. Execution is added with the extracted
// handler cores; this slice owns validation, lifetime and class.
type Definition struct {
	Type       string
	Version    int
	MinClass   string
	Target     Target
	DefaultTTL time.Duration
	MaxTTL     time.Duration

	// RequiresPreview stores a server-computed preview in the preconditions and
	// makes approval present that preview's digest.
	RequiresPreview bool

	// OwnerOnly makes approval need the workspace owner whatever the target and
	// class. An action on a non-agent target is owner-only already.
	OwnerOnly bool

	// Passkey says whether approving the action needs a passkey assertion. The
	// registry sets it per action type, never the proposer (B v9 S5, CHE-5415).
	// The zero value is "unset": the handler's registry test fails for it.
	Passkey Passkey

	// Template is the wording an approver reads for this action. Every action
	// registers one; the handler's registry test fails for a type without it.
	Template Template

	// Normalize is exported so a package that owns an executor can register
	// its action type.
	Normalize NormalizeFunc
}

// Passkey is a registry decision, not a proposal field.
type Passkey uint8

const (
	PasskeyUnset Passkey = iota
	PasskeyRequired
	PasskeyNotRequired
)

// RequiresPasskey is what the card and `multica decision get` read.
func (d *Definition) RequiresPasskey() bool { return d.Passkey == PasskeyRequired }

// Template is what a human reads before approving an action. Both fields are
// static text: the server writes the ask, so no agent-controlled string may
// reach it through a template. The before → after rows come from the action's
// preview, never from the template.
type Template struct {
	// Title names the action in plain words.
	Title string
	// Summary is one line saying what approval changes.
	Summary string
}

type NormalizeFunc func(params json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error)

// TTL resolves a requested lifetime (0 = default) within the action's maximum.
func (d *Definition) TTL(requested time.Duration) (time.Duration, error) {
	switch {
	case requested == 0:
		return d.DefaultTTL, nil
	case requested < 0 || requested > d.MaxTTL:
		return 0, invalid("expiry must be between 0 and %s", d.MaxTTL)
	}
	return requested, nil
}

var registry = map[string]*Definition{}

// Register adds an allowlisted action type; a repeat replaces the earlier one.
func Register(def *Definition) {
	def.DefaultTTL, def.MaxTTL = 24*time.Hour, 72*time.Hour
	registry[def.Type+"@"+strconv.Itoa(def.Version)] = def
}

func init() {
	Register(&Definition{Type: "agent.copy", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Copy an agent", Summary: "Creates a new agent with the same settings as the source agent."}, MinClass: "reversible", Normalize: normalizeAgentCopy})
	Register(&Definition{Type: "agent.env.patch", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Change an agent's environment variables", Summary: "Sets or removes environment variable keys on one agent; values are never shown."}, MinClass: "reversible", Normalize: normalizeEnvPatch})
	Register(&Definition{Type: "agent.pause", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Pause an agent", Summary: "Stops the agent from starting new runs until it is resumed."}, MinClass: "reversible", Normalize: normalizeAgentSchedulingChange("agent.pause")})
	Register(&Definition{Type: "agent.resume", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Resume an agent", Summary: "Lets a paused agent start new runs again."}, MinClass: "reversible", Normalize: normalizeAgentSchedulingChange("agent.resume")})
	Register(&Definition{Type: "issue.run_budget.raise", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Raise an issue's run cap", Summary: "Raises the run cap of the issue's open budget window."}, MinClass: "spend", Target: TargetIssue, Normalize: normalizeRunBudgetRaise})
	Register(&Definition{Type: "issue.project.set", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Move an issue to another project", Summary: "Moves the issue to the proposed project, which sets the workflow and run budget the issue follows."}, MinClass: "reversible", Target: TargetIssue, Normalize: normalizeIssueProjectSet})
	Register(&Definition{Type: "issue.run_budget.top_up", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Top up an issue's runs", Summary: "Resets the runs used in the issue's open budget window to zero."}, MinClass: "spend", Target: TargetIssue, Normalize: normalizeRunBudgetTopUp})
	Register(&Definition{Type: "issue.workflow.set", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Change an issue's workflow", Summary: "Moves the issue to another workflow, which gives it a wider run budget."}, MinClass: "spend", Target: TargetIssue, Normalize: normalizeIssueWorkflowSet})
	Register(&Definition{Type: "agent.instructions.set", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Replace an agent's instructions", Summary: "Replaces the agent's instructions with the proposed text."}, MinClass: "spend", Target: TargetAgent, RequiresPreview: true, OwnerOnly: true, Normalize: normalizeAgentInstructionsSet})
	Register(&Definition{Type: "agent.skills.set", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Replace an agent's skills", Summary: "Replaces the agent's skill list with the proposed list."}, MinClass: "spend", Target: TargetAgent, RequiresPreview: true, OwnerOnly: true, Normalize: normalizeAgentSkillsSet})
}

// Types lists every allowlisted action type, sorted and without versions.
func Types() []string {
	seen := map[string]bool{}
	for _, def := range registry {
		seen[def.Type] = true
	}
	return sortedKeys(seen)
}

// Definitions lists every registered action, sorted by type then version.
func Definitions() []*Definition {
	definitions := make([]*Definition, 0, len(registry))
	for _, definition := range registry {
		definitions = append(definitions, definition)
	}
	sort.Slice(definitions, func(i, j int) bool {
		if definitions[i].Type != definitions[j].Type {
			return definitions[i].Type < definitions[j].Type
		}
		return definitions[i].Version < definitions[j].Version
	})
	return definitions
}

func Lookup(typ string, version int) (*Definition, bool) {
	def, ok := registry[typ+"@"+strconv.Itoa(version)]
	return def, ok
}

// Normalize validates a request against the allowlist and returns its
// canonical params and name-sorted slots.
func Normalize(req Request) (Normalized, error) {
	def, ok := Lookup(req.Type, req.Version)
	if !ok {
		return Normalized{}, &Error{Code: CodeUnknownAction, Message: "unknown action type or version"}
	}
	params, slots, err := def.Normalize(req.Params, req.SecretSlots)
	if err != nil {
		return Normalized{}, err
	}
	canonical, err := Canonical(params)
	if err != nil {
		return Normalized{}, invalid("params are not canonicalizable: %v", err)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Slot < slots[j].Slot })
	return Normalized{Definition: def, Params: canonical, SecretSlots: slots}, nil
}

// decodeStrict refuses unknown fields and trailing data.
func decodeStrict(raw json.RawMessage, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return invalid("params: %v", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return invalid("params: trailing data")
	}
	return nil
}

func canonicalUUID(field, value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return "", invalid("%s must be a UUID", field)
	}
	return id.String(), nil
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

const envMaskedMarker = "****"

type AgentCopyParams struct {
	SourceAgentID        string                `json:"source_agent_id"`
	Name                 string                `json:"name"`
	RuntimeID            string                `json:"runtime_id"`
	Description          string                `json:"description"`
	Instructions         *string               `json:"instructions"`
	ConversationStarters []ConversationStarter `json:"conversation_starters"`
	AvatarURL            *string               `json:"avatar_url"`
	CustomArgs           []string              `json:"custom_args"`
	MaxConcurrentTasks   int32                 `json:"max_concurrent_tasks"`
	Model                string                `json:"model"`
	ThinkingLevel        string                `json:"thinking_level"`
	ServiceTier          string                `json:"service_tier"`
}

type ConversationStarter struct {
	Label  string `json:"label"`
	Prompt string `json:"prompt"`
}

func normalizeAgentCopy(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("agent.copy takes no secret slots")
	}
	var params AgentCopyParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.SourceAgentID, err = canonicalUUID("source_agent_id", params.SourceAgentID); err != nil {
		return nil, nil, err
	}
	if params.RuntimeID, err = canonicalUUID("runtime_id", params.RuntimeID); err != nil {
		return nil, nil, err
	}
	if params.Name == "" {
		return nil, nil, invalid("name is required")
	}
	if params.MaxConcurrentTasks < 0 || params.MaxConcurrentTasks > 50 {
		return nil, nil, invalid("max_concurrent_tasks must be 0 (default) or 1-50")
	}
	if params.ConversationStarters == nil {
		params.ConversationStarters = []ConversationStarter{}
	}
	if params.CustomArgs == nil {
		params.CustomArgs = []string{}
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

type EnvPatchParams struct {
	AgentID string            `json:"agent_id"`
	Set     map[string]string `json:"set"`
	Unset   []string          `json:"unset"`
}

func normalizeEnvPatch(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	var params EnvPatchParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.AgentID, err = canonicalUUID("agent_id", params.AgentID); err != nil {
		return nil, nil, err
	}
	if params.Set == nil {
		params.Set = map[string]string{}
	}
	for key, value := range params.Set {
		if !envKeyPattern.MatchString(key) {
			return nil, nil, invalid("set contains an invalid env key")
		}
		if value == envMaskedMarker {
			return nil, nil, invalid("set values must not be the masked marker")
		}
	}
	unset := map[string]bool{}
	for _, key := range params.Unset {
		if !envKeyPattern.MatchString(key) {
			return nil, nil, invalid("unset contains an invalid env key")
		}
		if _, dup := params.Set[key]; dup {
			return nil, nil, invalid("a key may not appear in both set and unset")
		}
		unset[key] = true
	}
	params.Unset = sortedKeys(unset)
	seen := map[string]bool{}
	out := make([]SecretSlot, 0, len(slots))
	for _, slot := range slots {
		if !envKeyPattern.MatchString(slot.Slot) || slot.Slot != slot.Target.EnvKey {
			return nil, nil, invalid("a secret slot must be named exactly like its env key")
		}
		agentID, err := canonicalUUID("secret slot target agent_id", slot.Target.AgentID)
		if err != nil {
			return nil, nil, err
		}
		if agentID != params.AgentID {
			return nil, nil, invalid("a secret slot must target the patched agent")
		}
		if seen[slot.Slot] {
			return nil, nil, invalid("duplicate secret slot")
		}
		seen[slot.Slot] = true
		if _, inParams := params.Set[slot.Slot]; inParams {
			return nil, nil, &Error{Code: CodeSecretInParams, Message: "a secret slot key must not carry a value in params"}
		}
		if unset[slot.Slot] {
			return nil, nil, invalid("a secret slot key must not be unset")
		}
		out = append(out, SecretSlot{Slot: slot.Slot, Target: SlotTarget{AgentID: agentID, EnvKey: slot.Slot}})
	}
	if len(params.Set) == 0 && len(params.Unset) == 0 && len(out) == 0 {
		return nil, nil, invalid("specify at least one key in set, unset or secret slots")
	}
	raw, err = json.Marshal(params)
	return raw, out, err
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// RunBudgetRaiseParams is the issue.run_budget.raise payload. WindowID is the
// snapshot's opaque window_id; RefusalID, when set, is part of the hashed params.
type RunBudgetRaiseParams struct {
	IssueID   string `json:"issue_id"`
	NewCap    int    `json:"new_cap"`
	WindowID  string `json:"window_id"`
	RefusalID string `json:"refusal_id,omitempty"`
}

// RunBudgetTopUpParams names the issue and the window the person saw. The window
// id is opaque: the executor compares it with the window in force.
type RunBudgetTopUpParams struct {
	IssueID   string `json:"issue_id"`
	WindowID  string `json:"window_id"`
	RefusalID string `json:"refusal_id,omitempty"`
}

// IssueProjectSetParams is the issue.project.set payload. The server fills
// IssueID from the request when the proposer sends only ProjectID.
type IssueProjectSetParams struct {
	IssueID   string `json:"issue_id"`
	ProjectID string `json:"project_id"`
}

func normalizeIssueProjectSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("issue.project.set takes no secret slots")
	}
	var params IssueProjectSetParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.IssueID, err = canonicalUUID("issue_id", params.IssueID); err != nil {
		return nil, nil, err
	}
	if params.ProjectID, err = canonicalUUID("project_id", params.ProjectID); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

func normalizeRunBudgetRaise(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("issue.run_budget.raise takes no secret slots")
	}
	var params RunBudgetRaiseParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.IssueID, err = canonicalUUID("issue_id", params.IssueID); err != nil {
		return nil, nil, err
	}
	if params.RefusalID != "" {
		if params.RefusalID, err = canonicalUUID("refusal_id", params.RefusalID); err != nil {
			return nil, nil, err
		}
	}
	if params.NewCap < 1 || params.NewCap > issueworkflow.MaxRunsPerIssueLimit {
		return nil, nil, invalid("new_cap must be 1-%d", issueworkflow.MaxRunsPerIssueLimit)
	}
	if err := checkRunBudgetWindowID(params.WindowID); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

func normalizeRunBudgetTopUp(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("issue.run_budget.top_up takes no secret slots")
	}
	var params RunBudgetTopUpParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.IssueID, err = canonicalUUID("issue_id", params.IssueID); err != nil {
		return nil, nil, err
	}
	if params.RefusalID != "" {
		if params.RefusalID, err = canonicalUUID("refusal_id", params.RefusalID); err != nil {
			return nil, nil, err
		}
	}
	if err := checkRunBudgetWindowID(params.WindowID); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

// checkRunBudgetWindowID: the server compares the id as a string, so only its
// own spelling is valid.
func checkRunBudgetWindowID(id string) error {
	if at, err := time.Parse(time.RFC3339Nano, id); err != nil || at.UTC().Format(time.RFC3339Nano) != id {
		return invalid("window_id must be a window_id read from the run budget")
	}
	return nil
}
