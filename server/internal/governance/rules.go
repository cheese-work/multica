package governance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

const RuleRepository = "cheese-work/multica-dotfiles"
const maxRuleManifestBytes = 128 << 10

var ruleIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
var repositoryPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
var secretPattern = regexp.MustCompile(`(?i)(-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|(?:api[_-]?key|secret|password|access[_-]?token)["']?\s*[:=]\s*["']?[^\s"',}]{8,}|\b(?:ghp_|mat_|sk-)[a-zA-Z0-9_-]{16,})`)

type RuleScope struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type RuleOverrideGrant struct {
	Scope  string   `json:"scope"`
	Fields []string `json:"fields"`
}

type Rule struct {
	ID                 string              `json:"id"`
	Scope              RuleScope           `json:"scope"`
	Text               string              `json:"text"`
	Triggers           []string            `json:"triggers,omitempty"`
	Check              string              `json:"check"`
	AllowedCorrections []string            `json:"allowed_corrections"`
	Mode               string              `json:"mode"`
	OverridableBy      []RuleOverrideGrant `json:"overridable_by,omitempty"`
	Replaces           string              `json:"replaces,omitempty"`
}

type RuleReference struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
}

type RuleRevision struct {
	SchemaVersion int             `json:"schema_version"`
	Rules         []Rule          `json:"rules"`
	References    []RuleReference `json:"references,omitempty"`
	Digest        string          `json:"-"`
	SourceDigest  string          `json:"-"`
}

type RuleContext struct {
	WorkspaceID string
	ProjectID   string
	SquadIDs    []string
	AgentID     string
}

type EffectiveRules struct {
	Effective   []Rule
	Inherited   []Rule
	Replaced    []Rule
	Conflicting []Rule
	Digest      string
}

func validRulePath(name string) bool {
	return name != "" && !strings.Contains(name, "\\") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && name != "." && !strings.HasPrefix(name, "../") && !strings.Contains(name, "/../")
}

// LoadRuleRevision reads one explicitly named manifest from the canonical repository.
// It does not discover documents, follow includes, or activate the revision.
func LoadRuleRevision(root, repository, name string) (RuleRevision, error) {
	if repository != RuleRepository || !validRulePath(name) || !strings.HasPrefix(name, "rules/") || path.Ext(name) != ".json" {
		return RuleRevision{}, errors.New("invalid canonical rule source")
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return RuleRevision{}, errors.New("invalid repository root")
	}
	contained, err := os.OpenRoot(root)
	if err != nil {
		return RuleRevision{}, err
	}
	defer contained.Close()
	current := root
	for _, part := range strings.Split(name, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return RuleRevision{}, errors.New("invalid rule source path")
		}
	}
	info, err := os.Lstat(current)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxRuleManifestBytes {
		return RuleRevision{}, errors.New("invalid rule manifest file")
	}
	pathInfo := info
	file, err := contained.Open(name)
	if err != nil {
		return RuleRevision{}, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxRuleManifestBytes || !os.SameFile(pathInfo, info) {
		return RuleRevision{}, errors.New("invalid opened rule manifest")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRuleManifestBytes+1))
	if err != nil {
		return RuleRevision{}, err
	}
	return ParseRuleRevision(data)
}

func ParseRuleRevision(data []byte) (RuleRevision, error) {
	if len(data) > maxRuleManifestBytes || secretPattern.Match(data) {
		return RuleRevision{}, errors.New("oversize or sensitive rule manifest")
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return RuleRevision{}, errors.New("invalid rule manifest")
	}
	if err := validateRuleJSONFields(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return RuleRevision{}, errors.New("invalid rule manifest")
	}
	if containsRuleSecret(decoded) {
		return RuleRevision{}, errors.New("sensitive rule manifest")
	}
	var revision RuleRevision
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&revision); err != nil {
		return RuleRevision{}, errors.New("invalid rule manifest")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuleRevision{}, errors.New("trailing rule manifest content")
	}
	if revision.SchemaVersion != 1 || len(revision.Rules) > 128 {
		return RuleRevision{}, errors.New("unsupported rule manifest")
	}
	revision.SourceDigest = fmt.Sprintf("%x", sha256.Sum256(data))
	seen := make(map[string]bool, len(revision.Rules))
	for index := range revision.Rules {
		rule := &revision.Rules[index]
		if !ruleIDPattern.MatchString(rule.ID) || seen[rule.ID] || !validScope(rule.Scope) || strings.TrimSpace(rule.Text) == "" || rule.Check == "" || !slices.Contains([]string{"shadow", "suggest", "correct"}, rule.Mode) {
			return RuleRevision{}, errors.New("invalid rule identity, scope, or mode")
		}
		seen[rule.ID] = true
		for _, action := range rule.AllowedCorrections {
			if !slices.Contains([]string{"mention_agent", "mention_squad", "assign_agent", "assign_squad", "set_status", "resolve_thread", "resume_run"}, action) {
				return RuleRevision{}, errors.New("unknown correction action")
			}
		}
		for _, grant := range rule.OverridableBy {
			if !slices.Contains([]string{"project", "squad", "agent"}, grant.Scope) || len(grant.Fields) == 0 {
				return RuleRevision{}, errors.New("invalid override grant")
			}
			for _, field := range grant.Fields {
				if !slices.Contains([]string{"text", "triggers", "check", "allowed_corrections", "mode"}, field) {
					return RuleRevision{}, errors.New("unknown override field")
				}
			}
		}
		if rule.Replaces != "" && (!ruleIDPattern.MatchString(rule.Replaces) || rule.Replaces == rule.ID) {
			return RuleRevision{}, errors.New("invalid replacement identity")
		}
	}
	for _, reference := range revision.References {
		if !repositoryPattern.MatchString(reference.Repository) || !commitPattern.MatchString(reference.Commit) || !validRulePath(reference.Path) || len(reference.SHA256) != 64 || strings.Contains(strings.ToLower(reference.Path), "secret") || strings.Contains(reference.Path, ".env") {
			return RuleRevision{}, errors.New("invalid context reference")
		}
		if _, err := hex.DecodeString(reference.SHA256); err != nil {
			return RuleRevision{}, errors.New("invalid context digest")
		}
	}
	slices.SortFunc(revision.Rules, func(left, right Rule) int { return strings.Compare(left.ID, right.ID) })
	slices.SortFunc(revision.References, func(left, right RuleReference) int {
		first, _ := json.Marshal(left)
		second, _ := json.Marshal(right)
		return bytes.Compare(first, second)
	})
	normalized, err := json.Marshal(revision)
	if err != nil {
		return RuleRevision{}, err
	}
	if secretPattern.Match(normalized) {
		return RuleRevision{}, errors.New("sensitive rule manifest")
	}
	revision.Digest = fmt.Sprintf("%x", sha256.Sum256(normalized))
	return revision, nil
}

func validateRuleJSONFields(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	fields := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			field, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := field.(string)
			if !ok {
				return errors.New("invalid rule manifest field")
			}
			name = strings.Map(func(character rune) rune {
				for {
					folded := unicode.SimpleFold(character)
					if folded <= character {
						return folded
					}
					character = folded
				}
			}, name)
			if fields[name] {
				return errors.New("duplicate rule manifest field")
			}
			fields[name] = true
		}
		if err := validateRuleJSONFields(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func containsRuleSecret(value any) bool {
	switch typed := value.(type) {
	case string:
		return secretPattern.MatchString(typed)
	case []any:
		for _, item := range typed {
			if containsRuleSecret(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if containsRuleSecret(key) || containsRuleSecret(item) {
				return true
			}
			if text, ok := item.(string); ok && secretPattern.MatchString(key+"="+text) {
				return true
			}
		}
	}
	return false
}

func validScope(scope RuleScope) bool {
	if !slices.Contains([]string{"workspace", "project", "squad", "agent"}, scope.Kind) {
		return false
	}
	_, err := uuid.Parse(scope.ID)
	return err == nil
}

func cloneRule(rule Rule) Rule {
	rule.Triggers = slices.Clone(rule.Triggers)
	rule.AllowedCorrections = slices.Clone(rule.AllowedCorrections)
	rule.OverridableBy = slices.Clone(rule.OverridableBy)
	for index := range rule.OverridableBy {
		rule.OverridableBy[index].Fields = slices.Clone(rule.OverridableBy[index].Fields)
	}
	return rule
}

func applies(scope RuleScope, subject RuleContext) bool {
	switch scope.Kind {
	case "workspace":
		return scope.ID == subject.WorkspaceID
	case "project":
		return scope.ID == subject.ProjectID
	case "squad":
		return slices.Contains(subject.SquadIDs, scope.ID)
	case "agent":
		return scope.ID == subject.AgentID
	}
	return false
}

func overrideAllowed(ancestor, replacement Rule) bool {
	level := map[string]int{"workspace": 0, "project": 1, "squad": 1, "agent": 2}
	if level[replacement.Scope.Kind] <= level[ancestor.Scope.Kind] {
		return false
	}
	for _, action := range replacement.AllowedCorrections {
		if !slices.Contains(ancestor.AllowedCorrections, action) {
			return false
		}
	}
	for _, grant := range replacement.OverridableBy {
		for _, field := range grant.Fields {
			allowed := false
			for _, inherited := range ancestor.OverridableBy {
				allowed = allowed || inherited.Scope == grant.Scope && slices.Contains(inherited.Fields, field)
			}
			if !allowed {
				return false
			}
		}
	}
	changed := map[string]bool{
		"text":                ancestor.Text != replacement.Text,
		"triggers":            !reflect.DeepEqual(ancestor.Triggers, replacement.Triggers),
		"check":               ancestor.Check != replacement.Check,
		"allowed_corrections": !reflect.DeepEqual(ancestor.AllowedCorrections, replacement.AllowedCorrections),
		"mode":                ancestor.Mode != replacement.Mode,
	}
	for _, grant := range ancestor.OverridableBy {
		if grant.Scope != replacement.Scope.Kind {
			continue
		}
		permitted := true
		for field, altered := range changed {
			if altered && !slices.Contains(grant.Fields, field) {
				permitted = false
			}
		}
		if permitted {
			return true
		}
	}
	return false
}

// ResolveRules computes an offline view. A conflict never grants an action or
// suppresses an unrelated rule; execution is handled by later deliveries.
func ResolveRules(revision RuleRevision, subject RuleContext) EffectiveRules {
	result := EffectiveRules{}
	applicable := make(map[string]Rule)
	workspaceActions := make(map[string]bool)
	for _, rule := range revision.Rules {
		if applies(rule.Scope, subject) {
			applicable[rule.ID] = rule
			if rule.Scope.Kind == "workspace" && rule.Replaces == "" {
				for _, action := range rule.AllowedCorrections {
					workspaceActions[action] = true
				}
			}
		}
	}
	replacements := make(map[string][]Rule)
	conflicted := make(map[string]bool)
	for _, rule := range revision.Rules {
		if !applies(rule.Scope, subject) || rule.Scope.Kind == "workspace" {
			continue
		}
		for _, action := range rule.AllowedCorrections {
			if !workspaceActions[action] {
				conflicted[rule.ID] = true
			}
		}
	}
	for _, rule := range revision.Rules {
		if !applies(rule.Scope, subject) || rule.Replaces == "" {
			continue
		}
		ancestor, found := applicable[rule.Replaces]
		if !found || !overrideAllowed(ancestor, rule) {
			conflicted[rule.ID] = true
			continue
		}
		if !conflicted[rule.ID] {
			replacements[ancestor.ID] = append(replacements[ancestor.ID], rule)
		}
	}
	for ancestorID, candidates := range replacements {
		if len(candidates) > 1 {
			conflicted[ancestorID] = true
			for _, candidate := range candidates {
				conflicted[candidate.ID] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, rule := range revision.Rules {
			if applies(rule.Scope, subject) && rule.Replaces != "" && conflicted[rule.Replaces] && !conflicted[rule.ID] {
				conflicted[rule.ID] = true
				changed = true
			}
		}
	}
	for _, rule := range revision.Rules {
		if !applies(rule.Scope, subject) {
			continue
		}
		rule = cloneRule(rule)
		if conflicted[rule.ID] {
			result.Conflicting = append(result.Conflicting, rule)
			continue
		}
		if rule.Replaces != "" {
			if conflicted[rule.Replaces] || len(replacements[rule.Replaces]) == 0 {
				result.Conflicting = append(result.Conflicting, rule)
				continue
			}
		}
		if len(replacements[rule.ID]) == 1 {
			result.Replaced = append(result.Replaced, rule)
			continue
		}
		if rule.Scope.Kind == "workspace" {
			result.Inherited = append(result.Inherited, cloneRule(rule))
		}
		rule.AllowedCorrections = slices.DeleteFunc(slices.Clone(rule.AllowedCorrections), func(action string) bool { return !workspaceActions[action] })
		if rule.Check != "missing_mention_v1" {
			rule.AllowedCorrections = nil
		}
		result.Effective = append(result.Effective, rule)
	}
	for index, left := range result.Effective {
		for right := index + 1; right < len(result.Effective); right++ {
			other := result.Effective[right]
			if left.Check != other.Check || left.ID == other.ID || len(left.AllowedCorrections) == 0 || len(other.AllowedCorrections) == 0 {
				continue
			}
			shared := false
			for _, action := range left.AllowedCorrections {
				shared = shared || slices.Contains(other.AllowedCorrections, action)
			}
			if !shared {
				conflicted[left.ID], conflicted[other.ID] = true, true
			}
		}
	}
	result.Effective = slices.DeleteFunc(result.Effective, func(rule Rule) bool {
		if conflicted[rule.ID] {
			result.Conflicting = append(result.Conflicting, rule)
			return true
		}
		return false
	})
	stableSubject := subject
	stableSubject.SquadIDs = slices.Clone(subject.SquadIDs)
	slices.Sort(stableSubject.SquadIDs)
	state, _ := json.Marshal(struct {
		Revision string
		Subject  RuleContext
		Rules    EffectiveRules
	}{revision.Digest, stableSubject, result})
	result.Digest = fmt.Sprintf("%x", sha256.Sum256(state))
	return result
}
