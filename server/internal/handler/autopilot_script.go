package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Autopilot script mode (CHE-4678 S9). The config names an allowlisted script
// and its args. It never carries a shell string. Limits live here, in one
// place; the handler and its tests read the same constants.
const (
	scriptMaxArgs   = 16
	scriptMaxArgLen = 256
)

var scriptNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type scriptConfig struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
}

func isValidAutopilotExecutionMode(mode string) bool {
	return mode == "create_issue" || mode == "run_only" || mode == "script"
}

// isAbsentJSON reports whether a raw field was omitted or sent as null.
func isAbsentJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// normalizeScriptConfig validates raw and returns the canonical JSON to store.
// Unknown fields are rejected, so a "command" or "shell" field cannot ride along.
func normalizeScriptConfig(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cfg scriptConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, errors.New("script_config must be an object with name and args")
	}
	if dec.More() {
		return nil, errors.New("script_config must be a single object")
	}
	if !scriptNamePattern.MatchString(cfg.Name) {
		return nil, errors.New("script_config.name must match " + scriptNamePattern.String())
	}
	if len(cfg.Args) > scriptMaxArgs {
		return nil, fmt.Errorf("script_config.args allows at most %d entries", scriptMaxArgs)
	}
	for _, a := range cfg.Args {
		if len(a) > scriptMaxArgLen {
			return nil, fmt.Errorf("script_config.args entries allow at most %d bytes", scriptMaxArgLen)
		}
		if strings.ContainsFunc(a, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, errors.New("script_config.args entries must not contain control characters")
		}
	}
	if cfg.Args == nil {
		cfg.Args = []string{}
	}
	return json.Marshal(cfg)
}

// resolveScriptConfig applies the mode/config pairing rule: script needs a
// valid config, every other mode must carry none. raw is the config the
// request supplied (absent = nil); stored is the value already on the row.
func resolveScriptConfig(mode string, raw json.RawMessage, supplied bool, stored []byte) ([]byte, error) {
	cfg := stored
	if supplied {
		cfg = nil
		if !isAbsentJSON(raw) {
			n, err := normalizeScriptConfig(raw)
			if err != nil {
				return nil, err
			}
			cfg = n
		}
	}
	switch {
	case mode == "script" && cfg == nil:
		return nil, errors.New("script_config is required when execution_mode is script")
	case mode != "script" && cfg != nil:
		return nil, errors.New("script_config is only allowed when execution_mode is script")
	}
	return cfg, nil
}

// claimScriptFor is the script the claim hands the daemon. Only script-mode
// autopilots carry one. A stored config that cannot be read yields an empty
// script, which the daemon rejects as not allowlisted: failing closed there is
// what keeps a broken row from becoming a model session.
func claimScriptFor(mode string, raw []byte) *scriptConfig {
	if mode != "script" {
		return nil
	}
	var cfg scriptConfig
	if json.Unmarshal(raw, &cfg) != nil {
		cfg = scriptConfig{}
	}
	return &cfg
}
