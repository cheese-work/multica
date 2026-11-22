// Package ghstack decides whether and how the platform links dependent PRs
// into GitHub stacks. This file is the mode setting: validation on write and
// the effective-mode resolver (CHE-4143 plan, "Effective mode").
package ghstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SettingKey is the workspace.settings key that holds the config:
// {"mode": "off|report|link", "repos": {"owner/repo": "off|report|link"}}.
const SettingKey = "github_stack_autolink"

// Mode orders off < report < link.
type Mode string

const (
	ModeOff    Mode = "off"
	ModeReport Mode = "report"
	ModeLink   Mode = "link"
)

func (m Mode) rank() int {
	switch m {
	case ModeLink:
		return 2
	case ModeReport:
		return 1
	}
	return 0
}

// parseMode accepts exactly the three mode names.
func parseMode(v any) (Mode, error) {
	s, _ := v.(string)
	switch m := Mode(s); m {
	case ModeOff, ModeReport, ModeLink:
		return m, nil
	}
	return "", errors.New("mode must be off, report or link")
}

// config is the parsed key. Repo keys are lowercased: GitHub names are case
// insensitive.
type config struct {
	mode  Mode
	repos map[string]Mode
}

func parseConfig(v any) (config, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return config{}, fmt.Errorf("%s must be an object", SettingKey)
	}
	cfg := config{mode: ModeReport}
	for k, val := range obj {
		switch k {
		case "mode":
			m, err := parseMode(val)
			if err != nil {
				return config{}, fmt.Errorf("%s.%w", SettingKey, err)
			}
			cfg.mode = m
		case "repos":
			repos, ok := val.(map[string]any)
			if !ok {
				return config{}, fmt.Errorf("%s.repos must be an object", SettingKey)
			}
			cfg.repos = make(map[string]Mode, len(repos))
			for repo, rv := range repos {
				if o, n, ok := strings.Cut(repo, "/"); !ok || o == "" || n == "" || strings.Contains(n, "/") {
					return config{}, fmt.Errorf("%s.repos key %q must be owner/repo", SettingKey, repo)
				}
				m, err := parseMode(rv)
				if err != nil {
					return config{}, fmt.Errorf("%s.repos[%q]: %w", SettingKey, repo, err)
				}
				key := strings.ToLower(repo)
				if prev, dup := cfg.repos[key]; !dup || m.rank() < prev.rank() {
					cfg.repos[key] = m // case-variant duplicates: the lowest wins
				}
			}
		default:
			return config{}, fmt.Errorf("%s has unknown field %q", SettingKey, k)
		}
	}
	return cfg, nil
}

// ValidateSetting checks the key in a workspace settings object. An absent
// key is valid; null is not. A value that is not an object is left to the
// other validators, as for ask_guard_mode.
func ValidateSetting(value any) error {
	settings, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	v, present := settings[SettingKey]
	if !present {
		return nil
	}
	_, err := parseConfig(v)
	return err
}

// EffectiveMode is rule 1 for one workspace and one repo ("owner/repo").
// Workspace off is the shutdown: a repo entry cannot raise it. Otherwise a
// repo entry wins, else the workspace mode; no key means report. Settings the
// resolver cannot read mean off: it must not guess a mode that writes.
func EffectiveMode(repo string, rawSettings []byte) Mode {
	if len(rawSettings) == 0 {
		return ModeReport
	}
	var settings map[string]any
	if err := json.Unmarshal(rawSettings, &settings); err != nil {
		return ModeOff
	}
	v, ok := settings[SettingKey]
	if !ok {
		return ModeReport
	}
	cfg, err := parseConfig(v)
	if err != nil {
		return ModeOff
	}
	if cfg.mode == ModeOff {
		return ModeOff
	}
	if m, ok := cfg.repos[strings.ToLower(repo)]; ok {
		return m
	}
	return cfg.mode
}

// FinalMode is rules 2–4: the lowest effective mode across the settings of
// every workspace bound to the installation, off when the env kill is set or
// no workspace is bound.
func FinalMode(repo string, boundWorkspaceSettings [][]byte, envKill bool) Mode {
	if envKill || len(boundWorkspaceSettings) == 0 {
		return ModeOff
	}
	final := ModeLink
	for _, raw := range boundWorkspaceSettings {
		if m := EffectiveMode(repo, raw); m.rank() < final.rank() {
			final = m
		}
	}
	return final
}

// EnvKill reports GITHUB_STACK_AUTOLINK=0, the off switch for every repo.
func EnvKill() bool { return os.Getenv("GITHUB_STACK_AUTOLINK") == "0" }
