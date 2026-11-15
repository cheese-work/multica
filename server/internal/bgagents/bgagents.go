package bgagents

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type Job string
type RunnerKind string
type Scope string
type Source string
type SlotName string

const (
	DMTitle      Job = "dm_title"
	QuickActions Job = "quick_actions"
	Compact      Job = "compact"
	Routing      Job = "routing"

	OutputOnly     RunnerKind = "output_only"
	ChatCompaction RunnerKind = "chat_compaction"
	WorkflowStep   RunnerKind = "workflow_step"

	WorkspaceScope Scope = "workspace"
	UserScope      Scope = "user"

	UserSource      Source = "user"
	WorkspaceSource Source = "workspace"
	NoAgent         Source = "none"

	Primary  SlotName = "primary"
	Fallback SlotName = "fallback"

	CodeUnknownJob       = "unknown_job"
	CodeUnknownField     = "unknown_field"
	CodeRoutingUserScope = "routing_user_scope"
	CodeInvalidAgentID   = "invalid_agent_id"
	CodeInvalidShape     = "invalid_shape"
)

// Spec is one row of the closed job catalog. A zero deadline means the job's
// existing rules apply (compaction limits, PR #989 routing rules).
type Spec struct {
	Runner                     RunnerKind
	ClaimDeadline, RunDeadline time.Duration
	UserScoped                 bool
}

// Catalog is the closed set of background jobs. Adding a job is a code change
// plus one row here.
var Catalog = map[Job]Spec{
	DMTitle:      {OutputOnly, 10 * time.Second, 60 * time.Second, true},
	QuickActions: {OutputOnly, 5 * time.Second, 20 * time.Second, true},
	Compact:      {ChatCompaction, 30 * time.Second, 0, true},
	Routing:      {WorkflowStep, 0, 0, false},
}

func SpecFor(j Job) (Spec, bool) { s, ok := Catalog[j]; return s, ok }

// Chain is two slots, no more. Statuses and Overrides belong to Routing only.
type Chain struct {
	AgentID, FallbackAgentID string
	Statuses                 []string
	Overrides                map[string]string
}
type Config map[Job]Chain

// Error carries a stable Code for the HTTP layer (400).
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(code, format string, args ...any) error {
	return &Error{code, fmt.Sprintf(format, args...)}
}

type rawChain struct {
	AgentID         *string           `json:"agent_id"`
	FallbackAgentID *string           `json:"fallback_agent_id"`
	Statuses        *[]string         `json:"statuses"`
	Overrides       map[string]string `json:"overrides"`
}

// Parse validates a background_agents value for scope: closed job keys, two
// slots, routing extras on routing only, routing absent from user scope.
func Parse(raw []byte, scope Scope) (Config, error) {
	var jobs map[string]json.RawMessage
	if err := json.Unmarshal(raw, &jobs); err != nil || jobs == nil {
		return nil, fail(CodeInvalidShape, "background_agents must be an object")
	}
	cfg := Config{}
	for key, val := range jobs {
		job := Job(key)
		spec, ok := SpecFor(job)
		if !ok {
			return nil, fail(CodeUnknownJob, "unknown background job %q", key)
		}
		if scope == UserScope && !spec.UserScoped {
			return nil, fail(CodeRoutingUserScope, "job %q is workspace-only", key)
		}
		chain, err := parseChain(job, val)
		if err != nil {
			return nil, err
		}
		cfg[job] = chain
	}
	return cfg, nil
}

func parseChain(job Job, val json.RawMessage) (Chain, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(val, &fields); err != nil || fields == nil {
		return Chain{}, fail(CodeInvalidShape, "job %q must be an object", job)
	}
	for k := range fields {
		switch k {
		case "agent_id", "fallback_agent_id":
		case "statuses", "overrides":
			if job == Routing {
				continue
			}
			fallthrough
		default:
			return Chain{}, fail(CodeUnknownField, "job %q has no field %q", job, k)
		}
	}
	var r rawChain
	if err := json.Unmarshal(val, &r); err != nil {
		return Chain{}, fail(CodeInvalidShape, "job %q: %v", job, err)
	}
	c := Chain{Overrides: r.Overrides}
	if r.Statuses != nil {
		c.Statuses = *r.Statuses
	}
	for _, id := range []struct {
		in  *string
		out *string
	}{{r.AgentID, &c.AgentID}, {r.FallbackAgentID, &c.FallbackAgentID}} {
		if id.in == nil {
			continue
		}
		if _, err := util.ParseUUID(*id.in); err != nil {
			return Chain{}, fail(CodeInvalidAgentID, "job %q: %q is not an agent id", job, *id.in)
		}
		*id.out = *id.in
	}
	for step, id := range c.Overrides {
		if _, err := util.ParseUUID(id); err != nil {
			return Chain{}, fail(CodeInvalidAgentID, "job %q: override %q: %q is not an agent id", job, step, id)
		}
	}
	return c, nil
}

type Candidate struct {
	Slot    SlotName
	AgentID string
}

// Resolution is the chain to try, in order, and where it came from. An empty
// Chain means the job's no-agent path.
type Resolution struct {
	Source Source
	Chain  []Candidate
}

// Resolve picks the chain for job: the user's if it names a primary agent, else
// the workspace's, else none. Ineligible slots are skipped. A user chain whose
// slots are all ineligible does not fall through to the workspace chain: the
// user's explicit choice is respected and the no-agent path runs.
// Routing is workspace-only, so the user config is ignored for it.
func Resolve(job Job, user, workspace Config, eligible func(Source, string) bool) Resolution {
	spec, ok := SpecFor(job)
	if !ok {
		return Resolution{Source: NoAgent}
	}
	if spec.UserScoped && user[job].AgentID != "" {
		return pick(UserSource, user[job], eligible)
	}
	if workspace[job].AgentID != "" {
		return pick(WorkspaceSource, workspace[job], eligible)
	}
	return Resolution{Source: NoAgent}
}

func pick(src Source, c Chain, eligible func(Source, string) bool) Resolution {
	r := Resolution{Source: src}
	for _, s := range []Candidate{{Primary, c.AgentID}, {Fallback, c.FallbackAgentID}} {
		if s.AgentID != "" && eligible(src, s.AgentID) {
			r.Chain = append(r.Chain, s)
		}
	}
	return r
}

// WorkspaceEligible is ENG-8: workspace-visible (not private), not archived,
// on a runtime. Checked at write time and again at resolve time.
func WorkspaceEligible(a db.Agent) bool {
	return a.Visibility == "workspace" && !a.ArchivedAt.Valid && a.RuntimeID.Valid
}

// Purpose is the agent_task_queue.purpose value for a job's tasks.
func Purpose(j Job) string { return "bg:" + string(j) }

// Eligible is the resolve-time check for one agent on job: ENG-8, plus, for an
// output_only job, a provider with a verified no-tool mode.
func Eligible(job Job, a db.Agent, provider string) bool {
	if !WorkspaceEligible(a) {
		return false
	}
	if spec, ok := SpecFor(job); ok && spec.Runner == OutputOnly {
		return agent.OutputOnlyIneligibleReason(provider) == ""
	}
	return true
}
