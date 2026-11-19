// Package capabilityrouting reads and validates the workspace setting
// capability_routing (CHE-7666): per issue type, the author agent and the
// ordered reviewer agents. Agents are capability profiles; the workflow step
// names the role and this table names who fills it.
package capabilityrouting

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// SettingsKey is the workspace settings key. The setting is unset by default,
// and an unset table routes nothing.
const SettingsKey = "capability_routing"

// Entry is the routing of one issue type.
type Entry struct {
	Author    string   `json:"author"`
	Reviewers []string `json:"reviewers"`
}

// Config maps a lowercase issue type (as in workflow_by_type) to its routing.
type Config map[string]Entry

// Validate checks a stored value's shape. That every id is a workspace agent
// needs the database and is checked by the caller with AgentIDs.
func Validate(raw []byte) error {
	_, err := Parse(raw)
	return err
}

// Parse validates raw and returns the table with every id in canonical
// lowercase form.
func Parse(raw []byte) (Config, error) {
	var table map[string]json.RawMessage
	if err := json.Unmarshal(raw, &table); err != nil || table == nil {
		return nil, fmt.Errorf("%s must be an object of issue type to {author, reviewers}", SettingsKey)
	}
	if len(table) == 0 {
		return nil, fmt.Errorf("%s must name at least one issue type; use null to remove it", SettingsKey)
	}
	cfg := make(Config, len(table))
	for key, body := range table {
		if key == "" || key != strings.ToLower(key) || strings.ContainsAny(key, " \t\r\n") {
			return nil, fmt.Errorf("%s key %q must be a lowercase issue type without spaces", SettingsKey, key)
		}
		entry, err := parseEntry(key, body)
		if err != nil {
			return nil, err
		}
		cfg[key] = entry
	}
	return cfg, nil
}

func parseEntry(key string, body json.RawMessage) (Entry, error) {
	var in struct {
		Author    *string  `json:"author"`
		Reviewers []string `json:"reviewers"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return Entry{}, fmt.Errorf("%s.%s must be {author, reviewers}: %w", SettingsKey, key, err)
	}
	author, ok := agentID(in.Author)
	if !ok {
		return Entry{}, fmt.Errorf("%s.%s.author must be an agent id (UUID)", SettingsKey, key)
	}
	if len(in.Reviewers) == 0 {
		return Entry{}, fmt.Errorf("%s.%s.reviewers must name at least one agent", SettingsKey, key)
	}
	entry := Entry{Author: author, Reviewers: make([]string, 0, len(in.Reviewers))}
	for _, raw := range in.Reviewers {
		id, ok := agentID(&raw)
		if !ok {
			return Entry{}, fmt.Errorf("%s.%s.reviewers holds %q, which is not an agent id (UUID)", SettingsKey, key, raw)
		}
		if id == author {
			return Entry{}, fmt.Errorf("%s.%s: the author %s cannot also be a reviewer", SettingsKey, key, id)
		}
		if slices.Contains(entry.Reviewers, id) {
			return Entry{}, fmt.Errorf("%s.%s.reviewers names %s twice", SettingsKey, key, id)
		}
		entry.Reviewers = append(entry.Reviewers, id)
	}
	return entry, nil
}

func agentID(raw *string) (string, bool) {
	if raw == nil {
		return "", false
	}
	var id pgtype.UUID
	if id.Scan(*raw) != nil || !id.Valid {
		return "", false
	}
	return strings.ToLower(*raw), true
}

// AgentIDs lists every distinct agent the table names, sorted.
func (c Config) AgentIDs() []string {
	var ids []string
	for _, entry := range c {
		for _, id := range append([]string{entry.Author}, entry.Reviewers...) {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	return ids
}

// FromSettings reads the table from a decoded workspace settings object. It is
// the lenient reader: an absent or malformed setting, or a malformed entry, is
// skipped so a bad value routes nothing instead of failing the caller.
func FromSettings(settings map[string]any) Config {
	value, ok := settings[SettingsKey].(map[string]any)
	if !ok {
		return Config{}
	}
	cfg := Config{}
	for key, body := range value {
		raw, err := json.Marshal(body)
		if err != nil {
			continue
		}
		entry, err := parseEntry(key, raw)
		if err != nil {
			continue
		}
		cfg[key] = entry
	}
	return cfg
}
