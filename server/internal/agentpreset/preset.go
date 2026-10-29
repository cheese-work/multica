// Package agentpreset resolves which named settings bundle (model, thinking
// level, service tier, context args) a run uses, and checks that a bundle is
// still valid for the agent it belongs to. It is pure: callers load the
// presets, labels and workflow step, and persist the outcome.
package agentpreset

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// BaseName is the reserved name for the agent row itself.
const BaseName = "base"

// Source is the selection level that picked a preset.
type Source string

const (
	SourceDM           Source = "dm"
	SourceIssueLabel   Source = "issue_label"
	SourceWorkflowStep Source = "workflow_step"
	SourceAgentDefault Source = "agent_default"
	SourceBase         Source = "base"
)

// Stale reasons recorded in Skip.Reason.
const (
	ReasonUnknownPreset = "unknown_preset"
	ReasonAmbiguous     = "ambiguous"
	ReasonContextArgs   = "context_args"
	ReasonThinkingLevel = "thinking_level"
	ReasonServiceTier   = "service_tier"
	ReasonModel         = "model"
	ReasonLabLock       = "lab_lock"
)

// Preset is one agent_preset row. A nil field inherits the agent row. For
// ContextArgs, nil inherits and a non-nil empty slice replaces the agent's
// context args with none.
type Preset struct {
	Name          string
	Model         *string
	ThinkingLevel *string
	ServiceTier   *string
	ContextArgs   []string
	IsDefault     bool
}

// Agent is the current agent row joined with its runtime provider.
type Agent struct {
	Provider      string
	Model         string
	ThinkingLevel string
	ServiceTier   string
}

// Labs maps "provider/model" to a lab name (MULTICA_VOTE_GATE_LABS).
type Labs map[string]string

// Settings are the effective run settings after merging a preset over the
// agent row. ContextArgs is nil when no preset sets them and non-nil empty
// when a preset clears them.
type Settings struct {
	Model         string
	ThinkingLevel string
	ServiceTier   string
	ContextArgs   []string
}

// Skip records a level that was passed over and why.
type Skip struct {
	Level  Source
	Name   string
	Reason string
}

// StepHandler is the workflow step handler for the task's status.
type StepHandler struct {
	Type   string // "agent" or "squad"
	ID     string
	Preset string // empty = none
}

// Task identifies the run being claimed.
type Task struct {
	AgentID      string
	SquadID      string
	IsLeaderTask bool
}

// Input is everything Resolve needs for one claim.
type Input struct {
	Agent       Agent
	Labs        Labs
	Presets     []Preset
	DM          *string // chat_session.preset_name; nil = inherit
	IssueLabels []string
	Step        *StepHandler
	Task        Task
}

// Result is the outcome of Resolve. Preset is nil when the agent row is used.
type Result struct {
	Preset   *Preset
	Name     string
	Source   Source
	Settings Settings
	Skips    []Skip
}

var (
	nameRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	claudeSizeRe  = regexp.MustCompile(`^[1-9][0-9]*[kKmM]?$`)
	reservedNames = map[string]bool{BaseName: true, "reset": true}
	// codexContextKeys are the only `-c key=int` overrides a preset may carry.
	codexContextKeys = map[string]bool{
		"model_context_window":           true,
		"model_auto_compact_token_limit": true,
	}
)

// ValidateName checks a preset name. "base" and "reset" are reserved for the
// agent row and the clear-to-inherit action.
func ValidateName(name string) error {
	if reservedNames[name] {
		return fmt.Errorf("preset name %q is reserved", name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("preset name must match %s", nameRe)
	}
	return nil
}

// ValidateContextArgs allows only context-window args for the provider:
// claude `--autocompact <size>`; codex `-c model_context_window=<int>` and
// `-c model_auto_compact_token_limit=<int>`, each at most once. Everything
// else (sandbox, roots, MCP, permissions, model flags) is rejected, as is any
// arg for another provider.
func ValidateContextArgs(provider string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	switch provider {
	case "claude":
		if len(args) != 2 || args[0] != "--autocompact" || !claudeSizeRe.MatchString(args[1]) {
			return fmt.Errorf("claude context_args must be exactly --autocompact <size>, got %q", args)
		}
		return nil
	case "codex":
		seen := map[string]bool{}
		if len(args)%2 != 0 {
			return fmt.Errorf("codex context_args must be -c key=int pairs, got %q", args)
		}
		for i := 0; i < len(args); i += 2 {
			key, val, ok := strings.Cut(args[i+1], "=")
			if args[i] != "-c" || !ok || !codexContextKeys[key] || seen[key] {
				return fmt.Errorf("codex context_args allow only -c model_context_window=<int> and -c model_auto_compact_token_limit=<int>, got %q", args[i:i+2])
			}
			if n, err := strconv.ParseInt(val, 10, 64); err != nil || n <= 0 {
				return fmt.Errorf("codex context_args %s needs a positive integer, got %q", key, val)
			}
			seen[key] = true
		}
		return nil
	default:
		return fmt.Errorf("provider %q takes no preset context_args", provider)
	}
}

// Effective merges a preset over the agent row and returns a stale reason, or
// "" when the preset is valid for the agent now. It runs at every claim with
// no cached result: the agent's runtime, model or lab may have changed since
// the preset was written.
func Effective(a Agent, labs Labs, p Preset) (Settings, string) {
	if ValidateContextArgs(a.Provider, p.ContextArgs) != nil {
		return Settings{}, ReasonContextArgs
	}
	if p.ThinkingLevel != nil && !agent.IsKnownThinkingValue(a.Provider, *p.ThinkingLevel) {
		return Settings{}, ReasonThinkingLevel
	}
	if p.ServiceTier != nil && !agent.IsKnownServiceTier(a.Provider, *p.ServiceTier) {
		return Settings{}, ReasonServiceTier
	}
	s := Settings{Model: a.Model, ThinkingLevel: a.ThinkingLevel, ServiceTier: a.ServiceTier}
	if p.Model != nil {
		if agent.ModelKnownIncompatibleWithProvider(a.Provider, *p.Model) {
			return Settings{}, ReasonModel
		}
		// Lab lock: against the current base, which may have moved. An
		// unlisted base has no lock; the vote gate already treats an unlisted
		// pair as not independent. An inherited model passes by construction.
		if baseLab := labs[a.Provider+"/"+a.Model]; baseLab != "" && labs[a.Provider+"/"+*p.Model] != baseLab {
			return Settings{}, ReasonLabLock
		}
		s.Model = *p.Model
	}
	if p.ThinkingLevel != nil {
		s.ThinkingLevel = *p.ThinkingLevel
	}
	if p.ServiceTier != nil {
		s.ServiceTier = *p.ServiceTier
	}
	if p.ContextArgs != nil {
		s.ContextArgs = p.ContextArgs
	}
	return s, ""
}

// Validate is the create/update-time check: the name, plus every claim-time
// check against the agent as it is now.
func Validate(a Agent, labs Labs, p Preset) error {
	if err := ValidateName(p.Name); err != nil {
		return err
	}
	if _, reason := Effective(a, labs, p); reason != "" {
		return fmt.Errorf("preset %q is not valid for this agent: %s", p.Name, reason)
	}
	return nil
}

// Resolve picks the preset for one claim, highest level first: DM, issue
// label, workflow step, agent default, base. A level whose candidate is
// unknown, ambiguous or stale is skipped and recorded; the run never fails
// because of a preset.
func Resolve(in Input) Result {
	byName := make(map[string]Preset, len(in.Presets))
	for _, p := range in.Presets {
		byName[p.Name] = p
	}
	var skips []Skip

	// try returns a Result when name is usable at level, else records a skip.
	try := func(level Source, name string) (Result, bool) {
		if name == BaseName {
			return base(in, level, skips), true
		}
		p, ok := byName[name]
		if !ok {
			skips = append(skips, Skip{Level: level, Name: name, Reason: ReasonUnknownPreset})
			return Result{}, false
		}
		settings, reason := Effective(in.Agent, in.Labs, p)
		if reason != "" {
			skips = append(skips, Skip{Level: level, Name: name, Reason: reason})
			return Result{}, false
		}
		return Result{Preset: &p, Name: name, Source: level, Settings: settings, Skips: skips}, true
	}

	if in.DM != nil {
		if r, ok := try(SourceDM, *in.DM); ok {
			return r
		}
	}
	switch len(in.IssueLabels) {
	case 0:
	case 1:
		if r, ok := try(SourceIssueLabel, in.IssueLabels[0]); ok {
			return r
		}
	default:
		skips = append(skips, Skip{Level: SourceIssueLabel, Reason: ReasonAmbiguous})
	}
	if name := stepPreset(in.Step, in.Task); name != "" {
		if r, ok := try(SourceWorkflowStep, name); ok {
			return r
		}
	}
	for _, p := range in.Presets {
		if p.IsDefault {
			if r, ok := try(SourceAgentDefault, p.Name); ok {
				return r
			}
			break
		}
	}
	return base(in, SourceBase, skips)
}

func base(in Input, level Source, skips []Skip) Result {
	return Result{
		Name:     BaseName,
		Source:   level,
		Settings: Settings{Model: in.Agent.Model, ThinkingLevel: in.Agent.ThinkingLevel, ServiceTier: in.Agent.ServiceTier},
		Skips:    skips,
	}
}

// stepPreset is the step's preset when its handler is this agent, or this run
// is that squad's leader task. A mention run on the same status, or a squad
// member task, does not get the step's preset.
func stepPreset(h *StepHandler, t Task) string {
	if h == nil || h.Preset == "" {
		return ""
	}
	switch h.Type {
	case "agent":
		if h.ID == t.AgentID {
			return h.Preset
		}
	case "squad":
		if t.IsLeaderTask && t.SquadID == h.ID {
			return h.Preset
		}
	}
	return ""
}

// MergeContextArgs returns the agent's custom args with the allow-listed
// context-window args removed and ctx appended. ctx nil means the preset sets
// none: custom is returned as is. Every other arg keeps its bytes and order.
func MergeContextArgs(provider string, custom, ctx []string) []string {
	if ctx == nil {
		return custom
	}
	out := make([]string, 0, len(custom)+len(ctx))
	for i := 0; i < len(custom); i++ {
		if n := contextArgLen(provider, custom[i:]); n > 0 {
			i += n - 1
			continue
		}
		out = append(out, custom[i])
	}
	return append(out, ctx...)
}

// ContextArgsOf returns the allow-listed context-window args in args, in
// order. It is never nil: no such arg is an empty slice.
func ContextArgsOf(provider string, args []string) []string {
	out := []string{}
	for i := 0; i < len(args); i++ {
		if n := contextArgLen(provider, args[i:]); n > 0 {
			out = append(out, args[i:i+n]...)
			i += n - 1
		}
	}
	return out
}

// contextArgLen is how many leading args of rest form one context-window arg
// for the provider, or 0 when rest does not start with one.
func contextArgLen(provider string, rest []string) int {
	switch provider {
	case "claude":
		if rest[0] == "--autocompact" && len(rest) > 1 {
			return 2
		}
		if strings.HasPrefix(rest[0], "--autocompact=") {
			return 1
		}
	case "codex":
		// Inherited args may spell the override any way codex accepts:
		// `-c k=v`, `--config k=v`, `-c=k=v`, `--config=k=v`, with TOML
		// whitespace around the key. A preset's own args stay strict (Validate).
		for _, flag := range []string{"-c", "--config"} {
			if rest[0] == flag && len(rest) > 1 && isCodexContextKV(rest[1]) {
				return 2
			}
			if kv, ok := strings.CutPrefix(rest[0], flag+"="); ok && isCodexContextKV(kv) {
				return 1
			}
		}
	}
	return 0
}

// isCodexContextKV reports whether kv is `key=value` for an allow-listed codex
// context key, ignoring whitespace around the key.
func isCodexContextKV(kv string) bool {
	key, _, ok := strings.Cut(kv, "=")
	return ok && codexContextKeys[strings.TrimSpace(key)]
}
