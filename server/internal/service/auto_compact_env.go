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
// (CHE-4331 Part 2; CHE-10000 decouples the run cap). It is never stored in
// agent env, and a key the agent's own env already names (any case) is left
// alone.
//
// The env is the run cap, a safety net inside one turn, not the DM compaction
// threshold. A chat task (a DM run) gets max(dm_run_cap, DM threshold), so the
// run never compacts below the point where Multica compacts the DM between
// turns. Any other task gets the built-in cap: 400000 tokens on a 1M-window
// model, 95% on a 200k one. A saved threshold_pct moves neither; only a saved
// token threshold can raise a chat task's cap.
func ClaudeCompactEnv(provider, model string, chat bool, workspaceSettings, agentConfig []byte, userEnv map[string]string) map[string]string {
	if provider != "claude" {
		return nil
	}
	rule := ResolveAutoCompact(workspaceSettings, agentConfig)
	if !rule.Enabled {
		return nil
	}
	large := strings.HasSuffix(strings.TrimSpace(model), "[1m]")
	var tokens int64
	switch {
	case large && chat:
		tokens = max(rule.DMRunCap1M, tokenThreshold(rule, rule.Tokens1M))
	case large:
		tokens = AutoCompactDefaultRunCap1M
	case chat:
		tokens = max(rule.DMRunCap200k, tokenThreshold(rule, rule.Tokens200k))
	default:
		tokens = AutoCompactDefaultRunCap200k
	}
	key, value := claudeCompactPctEnv, tokens*100/AutoCompactSmallWindow
	if large {
		key, value = claudeCompactWindowEnv, tokens
	}
	for k := range userEnv {
		if strings.EqualFold(k, key) {
			return nil
		}
	}
	return map[string]string{key: strconv.FormatInt(value, 10)}
}

// tokenThreshold is a saved token threshold, or 0 when a percent drives the
// DM threshold (a percent never moves the run cap).
func tokenThreshold(rule AutoCompact, tokens int64) int64 {
	if rule.ThresholdPct > 0 {
		return 0
	}
	return tokens
}
