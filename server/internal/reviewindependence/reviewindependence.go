// Package reviewindependence reads and validates the workspace setting
// review_independence (CHE-9319): which attribute of an author a reviewer must
// differ in, and how an agent or a commit identity gets its value. The fork
// holds the mechanism only; the values (labs, teams, anything) are workspace
// configuration.
package reviewindependence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// SettingsKey is the workspace settings key. Absent means no configured rule.
const SettingsKey = "review_independence"

// Rule is the only rule: a reviewer's value is not among the authors' values.
const Rule = "reviewer_value_not_in_author_values"

// Config is the stored setting.
type Config struct {
	// Attribute names what is compared, for example "lab". It appears in refusals.
	Attribute string `json:"attribute"`
	Rule      string `json:"rule"`
	// AgentValues maps a runtime "provider/model", or "provider/*", to a value.
	AgentValues map[string]string `json:"agent_values"`
	// CommitIdentities maps an exact email, or an exact email domain, to a value.
	CommitIdentities map[string]string `json:"commit_identities"`
}

// Parse validates raw and returns the setting with every key lowercased.
func Parse(raw []byte) (Config, error) {
	var in Config
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return Config{}, fmt.Errorf("%s must be {attribute, rule, agent_values, commit_identities}: %w", SettingsKey, err)
	}
	if strings.TrimSpace(in.Attribute) == "" {
		return Config{}, fmt.Errorf("%s.attribute must be a non-empty name", SettingsKey)
	}
	if in.Rule != Rule {
		return Config{}, fmt.Errorf("%s.rule must be %q", SettingsKey, Rule)
	}
	cfg := Config{Attribute: in.Attribute, Rule: in.Rule,
		AgentValues: make(map[string]string, len(in.AgentValues)), CommitIdentities: make(map[string]string, len(in.CommitIdentities))}
	for key, value := range in.AgentValues {
		provider, model, ok := strings.Cut(key, "/")
		if !ok || provider == "" || model == "" || strings.ContainsAny(key, " \t\r\n") {
			return Config{}, fmt.Errorf("%s.agent_values key %q must be provider/model or provider/*", SettingsKey, key)
		}
		if strings.TrimSpace(value) == "" {
			return Config{}, fmt.Errorf("%s.agent_values[%q] must be a non-empty value", SettingsKey, key)
		}
		cfg.AgentValues[strings.ToLower(key)] = value
	}
	for key, value := range in.CommitIdentities {
		if key == "" || strings.ContainsAny(key, " \t\r\n*<>") || strings.Count(key, "@") > 1 || strings.HasPrefix(key, "@") || strings.HasSuffix(key, "@") {
			return Config{}, fmt.Errorf("%s.commit_identities key %q must be an exact email or an exact email domain", SettingsKey, key)
		}
		if strings.TrimSpace(value) == "" {
			return Config{}, fmt.Errorf("%s.commit_identities[%q] must be a non-empty value", SettingsKey, key)
		}
		cfg.CommitIdentities[strings.ToLower(key)] = value
	}
	return cfg, nil
}

// Validate checks a stored value's shape.
func Validate(raw []byte) error {
	_, err := Parse(raw)
	return err
}

// FromSettings is the lenient reader: ok is false when the setting is absent or
// malformed, which means no configured rule.
func FromSettings(settings map[string]any) (Config, bool) {
	value, present := settings[SettingsKey]
	if !present {
		return Config{}, false
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return Config{}, false
	}
	cfg, err := Parse(raw)
	return cfg, err == nil
}

// FromSettingsJSON reads the setting from a workspace's settings column.
func FromSettingsJSON(raw []byte) (Config, bool) {
	var settings map[string]any
	if json.Unmarshal(raw, &settings) != nil {
		return Config{}, false
	}
	return FromSettings(settings)
}

// AgentValue is the value of the agent behind a runtime provider and model: the
// exact provider/model, else provider/*. "" means the setting does not list it.
func (c Config) AgentValue(provider, model string) string {
	provider = strings.ToLower(provider)
	if v, ok := c.AgentValues[provider+"/"+strings.ToLower(model)]; ok {
		return v
	}
	return c.AgentValues[provider+"/*"]
}

// PRCommit is one commit of a pull request: who is named on it and its message
// (for Co-authored-by trailers).
type PRCommit struct {
	SHA            string `json:"sha"`
	AuthorEmail    string `json:"author"`
	CommitterEmail string `json:"committer"`
	Message        string `json:"message,omitempty"`
}

// StoredCommits is what the PR snapshot keeps of a PR's commits: the head they
// were read at, and per commit only the identities, never the message text. The
// merge gate reads it, so it never calls the network.
type StoredCommits struct {
	Head    string     `json:"head"`
	Commits []PRCommit `json:"commits"`
}

// NewStoredCommits reduces each commit's message to its Co-authored-by trailers.
func NewStoredCommits(head string, commits []PRCommit) StoredCommits {
	out := StoredCommits{Head: head, Commits: make([]PRCommit, len(commits))}
	for i, c := range commits {
		var trailers []string
		for _, m := range coAuthorTrailer.FindAllStringSubmatch(c.Message, -1) {
			trailers = append(trailers, "Co-authored-by: x <"+m[1]+">")
		}
		c.Message = strings.Join(trailers, "\n")
		out.Commits[i] = c
	}
	return out
}

// CommitsAt parses a stored column and returns its commits when they were read
// at head; a column that is empty, unreadable or for another head has none.
func CommitsAt(raw []byte, head string) ([]PRCommit, bool) {
	if len(raw) == 0 || head == "" {
		return nil, false
	}
	var s StoredCommits
	if err := json.Unmarshal(raw, &s); err != nil || s.Head != head || len(s.Commits) == 0 {
		return nil, false
	}
	return s.Commits, true
}

var coAuthorTrailer = regexp.MustCompile(`(?im)^co-authored-by:[^<\n]*<([^<>\s]+)>\s*$`)

// CommitValues are the distinct values of the identities named on a commit:
// its author, its committer and its Co-authored-by trailers. An identity maps by
// its exact email, else by its exact domain (no suffix match). Identities the
// setting does not map add nothing.
func (c Config) CommitValues(commit PRCommit) []string {
	emails := []string{commit.AuthorEmail, commit.CommitterEmail}
	for _, m := range coAuthorTrailer.FindAllStringSubmatch(commit.Message, -1) {
		emails = append(emails, m[1])
	}
	var values []string
	for _, email := range emails {
		email = strings.ToLower(strings.TrimSpace(email))
		at := strings.LastIndex(email, "@")
		if at <= 0 || at == len(email)-1 {
			continue
		}
		value, ok := c.CommitIdentities[email]
		if !ok {
			value, ok = c.CommitIdentities[email[at+1:]]
		}
		if ok && !contains(values, value) {
			values = append(values, value)
		}
	}
	return values
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
