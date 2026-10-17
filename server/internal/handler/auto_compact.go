package handler

import (
	"encoding/json"
	"fmt"
	"math"
)

const (
	autoCompactMinPct     = 50
	autoCompactMaxPct     = 90
	autoCompactDefaultPct = 80
)

var errAutoCompactShape = fmt.Errorf("auto_compact must be an object of enabled (boolean) and threshold_pct (integer %d to %d), or null to inherit", autoCompactMinPct, autoCompactMaxPct)

// autoCompact is the effective DM auto-compact rule (CHE-1803).
type autoCompact struct {
	Enabled      bool
	ThresholdPct int
}

// resolveAutoCompact applies the defaults, then the workspace value, then the
// agent override; a field present in a later layer wins. Unparsable layers are
// skipped: both are validated on write, so this only guards stored legacy data.
func resolveAutoCompact(workspaceSettings, agentConfig []byte) autoCompact {
	rule := autoCompact{Enabled: true, ThresholdPct: autoCompactDefaultPct}
	for _, raw := range [][]byte{workspaceSettings, agentConfig} {
		var layer struct {
			AutoCompact *struct {
				Enabled      *bool    `json:"enabled"`
				ThresholdPct *float64 `json:"threshold_pct"`
			} `json:"auto_compact"`
		}
		if json.Unmarshal(raw, &layer) != nil || layer.AutoCompact == nil {
			continue
		}
		if layer.AutoCompact.Enabled != nil {
			rule.Enabled = *layer.AutoCompact.Enabled
		}
		if pct := layer.AutoCompact.ThresholdPct; pct != nil && validAutoCompactPct(*pct) {
			rule.ThresholdPct = int(*pct)
		}
	}
	return rule
}

func validAutoCompactPct(v float64) bool {
	return v == math.Trunc(v) && v >= autoCompactMinPct && v <= autoCompactMaxPct
}

// validateAutoCompactConfig checks the auto_compact key of a workspace settings
// or agent runtime_config object. An absent key or null (inherit) is valid.
func validateAutoCompactConfig(value any) error {
	config, ok := value.(map[string]any)
	if !ok || config["auto_compact"] == nil {
		return nil
	}
	rule, ok := config["auto_compact"].(map[string]any)
	if !ok {
		return errAutoCompactShape
	}
	if v, present := rule["enabled"]; present {
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("auto_compact.enabled must be a boolean")
		}
	}
	if v, present := rule["threshold_pct"]; present {
		pct, isNum := v.(float64)
		if !isNum || !validAutoCompactPct(pct) {
			return fmt.Errorf("auto_compact.threshold_pct must be an integer from %d to %d", autoCompactMinPct, autoCompactMaxPct)
		}
	}
	return nil
}

// validateCompactConfig is the single write-time check for the compact keys
// (compact_prompt, auto_compact) of workspace settings and agent runtime_config.
func validateCompactConfig(value any) error {
	if err := validateCompactPromptConfig(value); err != nil {
		return err
	}
	return validateAutoCompactConfig(value)
}
