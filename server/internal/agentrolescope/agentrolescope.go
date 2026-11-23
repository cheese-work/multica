// Package agentrolescope reads and validates the workspace setting
// agent_role_scope (CHE-9192): per agent, the classes of work it may be given by
// another agent. An agent with no entry is unrestricted, and an unset setting
// restricts nobody.
package agentrolescope

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// SettingsKey is the workspace settings key.
const SettingsKey = "agent_role_scope"

// Work classes. The first five are the seat of a planning agent; Execution and
// CodeReview are everything else.
const (
	Plan           = "plan"
	PlanReview     = "plan_review"
	Decision       = "decision"
	Debate         = "debate"
	SecurityCosign = "security_cosign"
	CodeReview     = "code_review"
	Execution      = "execution"
)

var classes = []string{Plan, PlanReview, Decision, Debate, SecurityCosign, CodeReview, Execution}

// typeClass maps an issue type prefix (the Conventional Commit type of the
// title) to its work class. A type not listed is execution work.
var typeClass = map[string]string{
	"plan": Plan, "design": Plan, "spec": Plan,
	"decision": Decision,
	"debate":   Debate,
	"security": SecurityCosign,
}

// Config is the stored setting.
type Config struct {
	// Agents maps a lowercase agent id to the classes that agent may be given.
	Agents map[string][]string `json:"agents"`
}

// Parse validates raw and returns the setting with canonical agent ids.
func Parse(raw []byte) (Config, error) {
	var in Config
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil || in.Agents == nil {
		return Config{}, fmt.Errorf("%s must be {agents: {agent id: [class]}}", SettingsKey)
	}
	cfg := Config{Agents: make(map[string][]string, len(in.Agents))}
	for rawID, list := range in.Agents {
		var id pgtype.UUID
		if id.Scan(rawID) != nil || !id.Valid {
			return Config{}, fmt.Errorf("%s.agents key %q is not an agent id (UUID)", SettingsKey, rawID)
		}
		if len(list) == 0 {
			return Config{}, fmt.Errorf("%s.agents.%s must list at least one class", SettingsKey, rawID)
		}
		for _, class := range list {
			if !slices.Contains(classes, class) {
				return Config{}, fmt.Errorf("%s.agents.%s: unknown class %q (known: %s)", SettingsKey, rawID, class, strings.Join(classes, ", "))
			}
		}
		cfg.Agents[id.String()] = slices.Compact(slices.Sorted(slices.Values(list)))
	}
	return cfg, nil
}

// FromSettings is the lenient reader: an absent or malformed setting restricts
// nobody instead of failing the caller.
func FromSettings(settings map[string]any) Config {
	value, ok := settings[SettingsKey]
	if !ok {
		return Config{}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return Config{}
	}
	cfg, err := Parse(raw)
	if err != nil {
		return Config{}
	}
	return cfg
}

// AgentIDs lists every agent the setting names, sorted.
func (c Config) AgentIDs() []string {
	ids := make([]string, 0, len(c.Agents))
	for id := range c.Agents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// ClassOf is the work class of an issue, read from its title:
//   - plan/design/spec, decision, debate and security(...) by type;
//   - review(plan|design|spec|decision|debate) is a plan review, review(security)
//     a co-sign, and any other review (review, review(code), review(pr)) a code
//     review;
//   - "security gate: ..." is the title of a security gate issue, the D8
//     co-sign request that the project's Review gates line creates;
//   - everything else is execution. A path in the title never makes work a
//     co-sign: the server holds no changed-file list to judge D8 paths by.
func (c Config) ClassOf(title string) string {
	typ, _ := issueworkflow.TitleType(title)
	if typ == "review" {
		return reviewClass(titleScope(title))
	}
	if class, ok := typeClass[typ]; ok {
		return class
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(title)), "security gate:") {
		return SecurityCosign
	}
	return Execution
}

// reviewClass is the class of a review(scope) issue.
func reviewClass(scope string) string {
	switch scope {
	case "plan", "design", "spec", "decision", "debate":
		return PlanReview
	case "security":
		return SecurityCosign
	}
	return CodeReview
}

// titleScope is the lowercase text in the first parentheses right after the type.
func titleScope(title string) string {
	open := strings.Index(title, "(")
	colon := strings.IndexAny(title, ":!")
	if open < 0 || (colon >= 0 && colon < open) {
		return ""
	}
	end := strings.Index(title[open:], ")")
	if end < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(title[open+1 : open+end]))
}

// ReviewClassOf is the class of a review of the issue: a planning-seat issue is
// reviewed as itself, any other issue is a code review.
func (c Config) ReviewClassOf(title string) string {
	if class := c.ClassOf(title); class != Execution {
		return class
	}
	return CodeReview
}

// Allows reports whether agentID may be given work of class. An agent with no
// entry is unrestricted.
func (c Config) Allows(agentID, class string) bool {
	list, scoped := c.Agents[strings.ToLower(agentID)]
	return !scoped || slices.Contains(list, class)
}

// Scope is the classes an agent is limited to, and whether it has an entry.
func (c Config) Scope(agentID string) ([]string, bool) {
	list, ok := c.Agents[strings.ToLower(agentID)]
	return list, ok
}
