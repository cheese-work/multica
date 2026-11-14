// Package guard checks a tool call (gh, later git and others) inside an agent
// run, before the real tool runs. A plugin is declarative data: which call it
// matches and what it may do (pass, fix, deny). Logic a predicate cannot say is
// a built-in action compiled into the binary and selected by name; v1 has no
// plugin scripts.
package guard

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
)

// Environment variables the daemon sets for a run. The policy path must point
// outside the working tree; the guard never reads policy from the repository.
const (
	PolicyEnv    = "MULTICA_GUARD_POLICY"
	PolicySHAEnv = "MULTICA_GUARD_POLICY_SHA256"
	LogEnv       = "MULTICA_GUARD_LOG"
)

const (
	ActionPass = "pass"
	ActionFix  = "fix"
	ActionDeny = "deny"
	// ActionShadow runs a built-in check that only logs; the call always runs.
	ActionShadow = "shadow"

	// Decision kinds written to the run's decision log.
	KindPass        = "pass"
	KindFixed       = "fixed"
	KindRefused     = "refused"
	KindPluginError = "plugin-error"
	KindShadowHit   = "shadow-hit"
)

// builtinFlags is the allowlist of flags each built-in action may add or edit.
// A plugin may narrow it with allow_flags and can never widen it.
var builtinFlags = map[string][]string{
	"gh-pr":  {"--draft", "--reviewer", "--add-reviewer", "--title"},
	"gh-api": {},
	// multica-wakeup only logs.
	"multica-wakeup": {},
}

// Match selects calls: the tool and the leading non-flag words after it.
type Match struct {
	Tool       string   `json:"tool"`
	Subcommand []string `json:"subcommand,omitempty"`
}

// Plugin is one declarative rule.
type Plugin struct {
	Name       string   `json:"name"`
	Match      Match    `json:"match"`
	Action     string   `json:"action"`
	Builtin    string   `json:"builtin,omitempty"`
	Message    string   `json:"message,omitempty"`
	FailClosed bool     `json:"fail_closed,omitempty"`
	AllowFlags []string `json:"allow_flags,omitempty"`
}

// ReviewRule names the GitHub reviewers for a repository. Repo is owner/name,
// owner/* or *; the most specific rule wins. GitHub is "required" (the default
// when reviewers are listed) or "skip" (review runs through Multica only, as for
// a repository we do not own).
type ReviewRule struct {
	Repo      string   `json:"repo"`
	GitHub    string   `json:"github,omitempty"`
	Reviewers []string `json:"reviewers,omitempty"`
}

// Policy is the data the daemon syncs from the server.
type Policy struct {
	Version int `json:"version"`
	// Verify forces `gh pr create` to draft and reads the PR back before
	// success. Default true.
	Verify  *bool        `json:"verify,omitempty"`
	Plugins []Plugin     `json:"plugins"`
	Review  []ReviewRule `json:"review,omitempty"`
	// Locked freezes every key this workspace layer sets against deeper layers.
	Locked bool    `json:"locked,omitempty"`
	Layers []Layer `json:"layers,omitempty"`

	lockdown bool // set by lockdownPolicy; never read from JSON
}

// Lockdown reports whether the guard refuses every call because the stored
// policy could not be delivered or verified.
func (p *Policy) Lockdown() bool { return p != nil && p.lockdown }

func (p *Policy) verify() bool { return p == nil || p.Verify == nil || *p.Verify }

// DefaultPolicy is the compiled policy: plugin 1 (PR tag, draft, reviewers) and
// the `gh api` write refusal, with no review rules.
func DefaultPolicy() *Policy {
	return &Policy{Version: 1, Plugins: []Plugin{
		{Name: "gh-pr", Match: Match{Tool: "gh", Subcommand: []string{"pr"}}, Action: ActionFix, Builtin: "gh-pr"},
		{Name: "gh-api", Match: Match{Tool: "gh", Subcommand: []string{"api"}}, Action: ActionDeny, Builtin: "gh-api"},
		{Name: "wakeup-create", Match: Match{Tool: "multica", Subcommand: []string{"issue", "wakeup", "create"}}, Action: ActionShadow, Builtin: "multica-wakeup"},
		{Name: "wakeup-update", Match: Match{Tool: "multica", Subcommand: []string{"issue", "wakeup", "update"}}, Action: ActionShadow, Builtin: "multica-wakeup"},
	}}
}

func (p *Policy) validate() error {
	if p.Version != 1 {
		return fmt.Errorf("policy version %d is not supported", p.Version)
	}
	for _, pl := range p.Plugins {
		if pl.Name == "" || pl.Match.Tool == "" {
			return fmt.Errorf("plugin %q needs a name and match.tool", pl.Name)
		}
		switch pl.Action {
		case ActionPass, ActionDeny:
		case ActionFix, ActionShadow:
			if pl.Builtin == "" {
				return fmt.Errorf("plugin %q: action %s needs a builtin", pl.Name, pl.Action)
			}
		default:
			return fmt.Errorf("plugin %q: unknown action %q", pl.Name, pl.Action)
		}
		if pl.Action == ActionDeny && pl.Builtin == "" && pl.Message == "" {
			return fmt.Errorf("plugin %q: action deny needs a message", pl.Name)
		}
		if pl.Builtin == "" {
			if len(pl.AllowFlags) > 0 {
				return fmt.Errorf("plugin %q: allow_flags needs a builtin", pl.Name)
			}
			continue
		}
		allowed, ok := builtinFlags[pl.Builtin]
		if !ok {
			return fmt.Errorf("plugin %q: unknown builtin %q", pl.Name, pl.Builtin)
		}
		for _, f := range pl.AllowFlags {
			if !contains(allowed, f) {
				return fmt.Errorf("plugin %q: flag %s is outside the allowlist of builtin %s", pl.Name, f, pl.Builtin)
			}
		}
	}
	for i := range p.Layers {
		if err := p.Layers[i].validate(); err != nil {
			return fmt.Errorf("layer %d: %w", i, err)
		}
	}
	for _, r := range p.Review {
		if r.Repo == "" {
			return fmt.Errorf("review rule without repo")
		}
		if r.GitHub != "" && r.GitHub != "required" && r.GitHub != "skip" {
			return fmt.Errorf("review rule %s: github must be required or skip, not %q", r.Repo, r.GitHub)
		}
	}
	return nil
}

// ParsePolicy decodes and validates a policy document. Unknown fields (for
// example a script) are an error.
func ParsePolicy(raw []byte) (*Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("guard policy: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("guard policy: %w", err)
	}
	return &p, nil
}

// LoadPolicy reads the policy file and accepts it only when its SHA-256 equals
// wantSHA. A policy without a digest is never accepted. Unknown fields (for
// example a script) are an error.
func LoadPolicy(file, wantSHA string) (*Policy, error) {
	if wantSHA == "" {
		return nil, fmt.Errorf("guard policy has no digest")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(wantSHA))) != 1 {
		return nil, fmt.Errorf("guard policy digest %s does not match the expected digest", got[:12])
	}
	p, err := ParsePolicy(raw)
	if err != nil {
		return nil, err
	}
	return p, nil // layers stay: the engine resolves them for each call's target
}

const lockdownMessage = "the workspace guard policy is unavailable or failed verification; every git and gh call is refused for this run until it is fixed"

// lockdownPolicy stands in for a policy that was set but cannot be loaded or
// verified. The stored rules are unknown, so any guarded call could be one of
// them (a fail_closed plugin on git, say); the guard refuses every call.
func lockdownPolicy() *Policy { return &Policy{Version: 1, lockdown: true} }

// PolicyFromEnv returns the daemon-provided policy. With no policy set it
// returns the compiled default. A policy that is set but does not verify fails
// closed: every guarded call is refused and warn explains why. Nothing is read from
// the working tree.
func PolicyFromEnv(getenv func(string) string) (pol *Policy, warn string) {
	file := getenv(PolicyEnv)
	if file == "" {
		return DefaultPolicy(), ""
	}
	p, err := LoadPolicy(file, getenv(PolicySHAEnv))
	if err != nil {
		return lockdownPolicy(), "guard: policy unavailable, refusing every git and gh call: " + err.Error()
	}
	return p, ""
}

// reviewFor returns the most specific review rule for repo (owner/name), or nil.
func (p *Policy) reviewFor(repo string) *ReviewRule {
	best, bestRank := (*ReviewRule)(nil), -1
	owner, _, _ := strings.Cut(repo, "/")
	for i := range p.Review {
		r := &p.Review[i]
		rank := -1
		switch {
		case strings.EqualFold(r.Repo, repo):
			rank = 2
		case path.Base(r.Repo) == "*" && strings.EqualFold(path.Dir(r.Repo), owner) && strings.Contains(r.Repo, "/"):
			rank = 1
		case r.Repo == "*":
			rank = 0
		}
		if rank > bestRank {
			best, bestRank = r, rank
		}
	}
	return best
}

// githubReviewers lists the logins to request on GitHub for repo.
func (p *Policy) githubReviewers(repo string) []string {
	r := p.reviewFor(repo)
	if r == nil || r.GitHub == "skip" {
		return nil
	}
	return r.Reviewers
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
