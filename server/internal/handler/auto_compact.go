package handler

import (
	"fmt"

	"github.com/multica-ai/multica/server/internal/service"
)

var errAutoCompactShape = fmt.Errorf("auto_compact must be an object of enabled (boolean) and threshold_pct (integer %d to %d), or null to inherit", service.AutoCompactMinPct, service.AutoCompactMaxPct)

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
		if !isNum || !service.ValidAutoCompactPct(pct) {
			return fmt.Errorf("auto_compact.threshold_pct must be an integer from %d to %d", service.AutoCompactMinPct, service.AutoCompactMaxPct)
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
