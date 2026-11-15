package workflowlint

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// Agent is what the agent and runtime rules need to know about one handler
// agent. The caller reads it from the database; this package does no I/O.
type Agent struct {
	Provider       string // runtime provider, for example "codex"
	Model          string // agent model, may be empty
	RuntimeID      string
	RuntimeVersion string // as the runtime reports it, for example "codex-cli 0.155.1"
	RuntimeOnline  bool
	Archived       bool
	Paused         bool // scheduling paused
	Hidden         bool // the caller may not see this agent or its runtime
}

// Usage says where a workflow is used (WF013).
type Usage struct {
	Projects             int
	ReferencedByWorkflow bool // another workflow's sub-workflow step names it
}

// Env is the outside state a lint run may look at. The zero value lints the
// structure only: each nil field turns its rules off.
type Env struct {
	// Agents holds every agent the workflow's handlers name, by handler ID.
	// An ID missing from a non-nil map is a deleted agent. Nil skips WF004-006.
	Agents map[string]Agent
	// Live turns on WF012, which reads runtime state that is stale at save time.
	Live bool
	// Usage turns on WF013. Nil skips it.
	Usage *Usage
	// Statuses is the workspace status catalog, status key to display name.
	// It turns on WF009 and WF010, which read step instructions. Nil skips them.
	Statuses map[string]string
}

// The two JSON files below are the only place that names a qualified runtime
// or a lab. A change to either is a reviewed PR (CHE-3658).

//go:embed qualified_runtimes.json
var qualifiedJSON []byte

//go:embed lab_map.json
var labJSON []byte

type qualifiedEntry struct {
	Version   string `json:"version"`
	RuntimeID string `json:"runtime_id,omitempty"` // empty: any runtime of the provider
	Note      string `json:"note,omitempty"`
}

// qualifiedRuntimes lists, per provider, the runtime versions that were
// qualified to write. Only recorded qualifications belong here. An empty
// runtime_id means any runtime of the provider.
type qualifiedRuntimes map[string][]qualifiedEntry

var qualified = mustLoad[qualifiedRuntimes](qualifiedJSON)

type labMap struct {
	Providers     map[string]string `json:"providers"`
	ModelPrefixes map[string]string `json:"model_prefixes"`
}

var labs = mustLoad[labMap](labJSON)

func mustLoad[T any](b []byte) T {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		panic(fmt.Sprintf("workflowlint: embedded config: %v", err))
	}
	return v
}

var versionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// version pulls the version number out of what a runtime reports.
func version(reported string) string { return versionRe.FindString(reported) }

// qualifies reports whether the agent's runtime may write. It fails closed:
// a provider with no entry, or no entry matching its exact version, is not
// qualified.
func (q qualifiedRuntimes) qualifies(a Agent) bool {
	v := version(a.RuntimeVersion)
	for _, e := range q[a.Provider] {
		if (v != "" && v == e.Version) && (e.RuntimeID == "" || e.RuntimeID == a.RuntimeID) {
			return true
		}
	}
	return false
}

// of returns the agent's lab. The model decides first, since one provider can
// run models of any lab; the provider decides when the model is unknown.
func (m labMap) of(a Agent) (string, bool) {
	model := strings.ToLower(a.Model)
	for prefix, lab := range m.ModelPrefixes {
		if strings.HasPrefix(model, prefix) {
			return lab, true
		}
	}
	lab, ok := m.Providers[strings.ToLower(a.Provider)]
	return lab, ok
}

// LintWith is Lint plus the agent, runtime and usage rules (WF004-006, 009, 010, 012, 013).
func LintWith(def issueworkflow.Definition, env Env) []Finding {
	l := linter{def: def, reached: reachedStatuses(def)}
	for _, s := range def.Steps {
		l.lintStep(s)
		l.lintHandler(s, env)
		l.lintInstructions(s, env)
	}
	l.lintRunBudget()
	l.lintLabs(env)
	l.lintUsage(env)
	return l.findings
}

// lintHandler runs WF004, WF005 and WF012 on a step's agent handler.
func (l *linter) lintHandler(s issueworkflow.Step, env Env) {
	if env.Agents == nil || s.Handler.Type != issueworkflow.HandlerAgent || s.Handler.ID == "" {
		return
	}
	a, ok := env.Agents[s.Handler.ID]
	if ok && a.Hidden {
		return // the caller may not learn this agent's runtime or state
	}
	switch {
	case !ok:
		l.add("WF004", LevelError, &s, fmt.Sprintf("The agent %q of %q does not exist.", s.Handler.ID, s.StatusKey),
			"Pick a live agent as the handler.")
		return
	case a.Archived:
		l.add("WF004", LevelError, &s, fmt.Sprintf("The agent of %q is archived.", s.StatusKey), "Pick a live agent as the handler.")
	case a.Paused:
		l.add("WF004", LevelError, &s, fmt.Sprintf("The agent of %q has scheduling paused, so it never claims a run.", s.StatusKey),
			"Resume scheduling for the agent or pick another agent.")
	}
	if !qualified.qualifies(a) {
		l.add("WF005", LevelError, &s, fmt.Sprintf("The %s runtime of %q (version %q) is not write-qualified.", a.Provider, s.StatusKey, a.RuntimeVersion),
			"Pick an agent on a qualified runtime, or qualify the runtime in qualified_runtimes.json by a reviewed PR.")
	}
	if env.Live && !a.RuntimeOnline {
		l.add("WF012", LevelWarning, &s, fmt.Sprintf("The runtime of %q is offline.", s.StatusKey), "Bring the runtime online or pick another agent.")
	}
}

// agentReview is the custom status key of the agent review step.
const agentReview = "agent_review"

// lintLabs is WF006 (RGS-1): the author and the reviewer must come from
// different labs. It only looks at workflows that have both an in_progress and
// an agent_review step, each with a concrete agent. An unknown lab warns.
func (l *linter) lintLabs(env Env) {
	author, review := l.agentOf(issuestatus.InProgress, env), l.agentOf(agentReview, env)
	if author == nil || review == nil {
		return
	}
	rs := l.stepOf(agentReview)
	la, okA := labs.of(*author)
	lr, okR := labs.of(*review)
	switch {
	case !okA || !okR:
		l.add("WF006", LevelWarning, rs, "lab unknown for the in_progress or agent_review agent, so RGS-1 is not checked.",
			"Add the provider or model to lab_map.json by a reviewed PR.")
	case la == lr:
		l.add("WF006", LevelError, rs, fmt.Sprintf("in_progress and agent_review are both handled by %s agents (RGS-1).", la),
			"Give agent_review a handler from a different lab than in_progress.")
	}
}

func (l *linter) stepOf(key string) *issueworkflow.Step {
	for i := range l.def.Steps {
		if l.def.Steps[i].StatusKey == key {
			return &l.def.Steps[i]
		}
	}
	return nil
}

// agentOf returns the agent handling the step, or nil when the step is
// missing, has no concrete agent, or the agent is unknown to env.
func (l *linter) agentOf(key string, env Env) *Agent {
	s := l.stepOf(key)
	if s == nil || env.Agents == nil || s.Handler.Type != issueworkflow.HandlerAgent {
		return nil
	}
	a, ok := env.Agents[s.Handler.ID]
	if !ok || a.Hidden {
		return nil
	}
	return &a
}

// lintUsage is WF013. The Default workflow is used by every project that picks none.
func (l *linter) lintUsage(env Env) {
	if u := env.Usage; u != nil && u.Projects == 0 && !u.ReferencedByWorkflow && !l.def.IsDefault {
		l.add("WF013", LevelInfo, nil, "No project uses this workflow and no other workflow refers to it.",
			"Attach it to a project or delete it.")
	}
}
