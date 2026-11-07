package handler

import (
	"encoding/json"
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
	// null is the stored mark of a percent replaced by a token limit (see
	// clearStalePct); a whole-settings save sends it back unchanged.
	if v, present := rule["threshold_pct"]; present && v != nil {
		pct, isNum := v.(float64)
		if !isNum || !service.ValidAutoCompactPct(pct) {
			return fmt.Errorf("auto_compact.threshold_pct must be an integer from %d to %d", service.AutoCompactMinPct, service.AutoCompactMaxPct)
		}
	}
	for key, max := range map[string]int64{
		"threshold_tokens_200k": service.AutoCompactMaxTokens200k,
		"threshold_tokens_1m":   service.AutoCompactMaxTokens1M,
	} {
		if v, present := rule[key]; present {
			n, isNum := v.(float64)
			if !isNum || !service.ValidAutoCompactTokens(n, max) {
				return fmt.Errorf("auto_compact.%s must be an integer from %d to %d", key, service.AutoCompactMinTokens, max)
			}
		}
	}
	return nil
}

// validateCompactConfig is the single write-time check for the compact keys
// (compact_prompt, auto_compact) and the harness_profile key of workspace settings and agent runtime_config.
func validateCompactConfig(value any) error {
	if err := validateCompactPromptConfig(value); err != nil {
		return err
	}
	if err := validateAutoCompactConfig(value); err != nil {
		return err
	}
	return validateHarnessProfileConfig(value)
}

// validateAutoCompactPatch checks a PATCH body: like a stored object, but a
// null threshold_pct is not a value a client may send.
func validateAutoCompactPatch(incoming any) error {
	if rule, ok := incoming.(map[string]any); ok {
		if v, present := rule["threshold_pct"]; present && v == nil {
			return fmt.Errorf("auto_compact.threshold_pct must be an integer from %d to %d", service.AutoCompactMinPct, service.AutoCompactMaxPct)
		}
	}
	return validateAutoCompactConfig(map[string]any{"auto_compact": incoming})
}

// clearStalePct makes a saved token limit effective. A patch merges into the
// stored object, so a percent saved earlier would keep winning over the token
// limit being saved now; the patch therefore sets threshold_pct to null unless
// it names a percent itself. Input is already validated.
func clearStalePct(patch json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(patch, &fields) != nil {
		return patch
	}
	_, hasPct := fields["threshold_pct"]
	_, has200k := fields["threshold_tokens_200k"]
	_, has1m := fields["threshold_tokens_1m"]
	if hasPct || (!has200k && !has1m) {
		return patch
	}
	fields["threshold_pct"] = json.RawMessage("null")
	out, err := json.Marshal(fields)
	if err != nil {
		return patch
	}
	return out
}
