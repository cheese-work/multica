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

// Definition is one allowlisted action. Execution is added with the extracted
// handler cores; this slice owns validation, lifetime and class.
type Definition struct {
	Type       string
	Version    int
	MinClass   string
	DefaultTTL time.Duration
	MaxTTL     time.Duration

	normalize func(params json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error)
}

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

func register(def *Definition) {
	def.DefaultTTL, def.MaxTTL = 24*time.Hour, 72*time.Hour
	registry[def.Type+"@"+strconv.Itoa(def.Version)] = def
}

func init() {
	register(&Definition{Type: "agent.copy", Version: 1, MinClass: "reversible", normalize: normalizeAgentCopy})
	register(&Definition{Type: "agent.env.patch", Version: 1, MinClass: "reversible", normalize: normalizeEnvPatch})
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
	params, slots, err := def.normalize(req.Params, req.SecretSlots)
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
