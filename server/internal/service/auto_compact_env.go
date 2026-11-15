package service

import (
	"strconv"
	"strings"
)

const (
	claudeCompactWindowEnv = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	claudeCompactPctEnv    = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"
)

// ClaudeCompactEnv is the compaction env a Claude Code run gets at claim time
// (CHE-4331 Part 2), from the same auto_compact setting that drives DM
// compaction. It is never stored in agent env, and a key the agent's own env
// already names (any case) is left alone. A run cannot be idle-detected, so a
// 1M-window model without a saved token value compacts at the 400k cap.
func ClaudeCompactEnv(provider, model string, workspaceSettings, agentConfig []byte, userEnv map[string]string) map[string]string {
	if provider != "claude" {
		return nil
	}
	rule := ResolveAutoCompact(workspaceSettings, agentConfig)
	if !rule.Enabled {
		return nil
	}
	key, value := claudeCompactPctEnv, int64(0)
	if strings.HasSuffix(strings.TrimSpace(model), "[1m]") {
		key = claudeCompactWindowEnv
		switch {
		case rule.ThresholdPct > 0:
			value = min(rule.ThresholdTokens(1_000_000), AutoCompactMaxTokens1M)
		case rule.Tokens1MSet:
			value = rule.Tokens1M
		default:
			value = AutoCompactMaxTokens1M
		}
	} else if rule.ThresholdPct > 0 {
		value = int64(rule.ThresholdPct)
	} else {
		value = rule.Tokens200k * 100 / AutoCompactSmallWindow
	}
	for k := range userEnv {
		if strings.EqualFold(k, key) {
			return nil
		}
	}
	return map[string]string{key: strconv.FormatInt(value, 10)}
}
