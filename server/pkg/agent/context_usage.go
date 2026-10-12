package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type ModelRequestUsage struct {
	Provider            string `json:"provider"`
	Model               string `json:"model"`
	InputTokens         int64  `json:"input_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheWriteTokens    int64  `json:"cache_write_tokens"`
	ContextWindow       int64  `json:"context_window"`
	CLICompactionTokens int64  `json:"cli_compaction_tokens"`
}

func claudeRequestUsage(message claudeSDKMessage, model string) *ModelRequestUsage {
	if message.ParentToolUseID != "" {
		return nil
	}
	var content claudeMessageContent
	if json.Unmarshal(message.Message, &content) != nil || content.Usage == nil || content.Model == "" {
		return nil
	}
	window := int64(0)
	if strings.HasPrefix(content.Model, "claude-") {
		window = 200000
		if strings.Contains(model, "[1m]") {
			window = 1000000
		}
	}
	return &ModelRequestUsage{Provider: "claude", Model: content.Model, InputTokens: content.Usage.InputTokens, CacheReadTokens: content.Usage.CacheReadInputTokens, CacheWriteTokens: content.Usage.CacheCreationInputTokens, ContextWindow: window}
}

func claudeCompactionTokens(window int64, env []string, cwd string, args []string, model string) int64 {
	if window <= 0 {
		return 0
	}
	compactWindow := min(window, int64(1000000))
	if value, found := lastClaudeEnvValue(env, "CLAUDE_CODE_MAX_CONTEXT_TOKENS"); found && value != "" {
		return 0
	}
	if value, found := lastClaudeEnvValue(env, "CLAUDE_CODE_DISABLE_1M_CONTEXT"); found && value == "1" {
		compactWindow = min(compactWindow, int64(200000))
	}
	canonicalModel := strings.TrimSuffix(model, "[1m]")
	if separator := strings.LastIndexAny(canonicalModel, "-@"); separator >= 0 && len(canonicalModel[separator+1:]) == 8 {
		if _, err := strconv.ParseUint(canonicalModel[separator+1:], 10, 32); err == nil {
			canonicalModel = canonicalModel[:separator]
		}
	}
	selectedModel := ""
	for index, arg := range args {
		if arg == "--model" && index+1 < len(args) {
			selectedModel = args[index+1]
		}
		if value, found := strings.CutPrefix(arg, "--model="); found {
			selectedModel = value
		}
	}
	nativeExtended := false
	for _, prefix := range []string{"claude-opus-4-7", "claude-opus-4-8", "claude-opus-5", "claude-sonnet-5", "claude-haiku-5-5", "claude-fable"} {
		nativeExtended = nativeExtended || strings.HasPrefix(canonicalModel, prefix)
	}
	if !nativeExtended && !strings.Contains(selectedModel, "[1m]") {
		compactWindow = min(compactWindow, int64(200000))
	}
	configDir, err := claudeConfigDir(env, cwd)
	if err != nil {
		return 0
	}
	for _, path := range []string{filepath.Join(configDir, "settings.json"), filepath.Join(cwd, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json"), "/etc/claude-code/managed-settings.json"} {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0
		}
		var settings struct {
			AutoCompactWindow *int64 `json:"autoCompactWindow"`
			ModelSettings     map[string]struct {
				AutoCompactWindow *int64 `json:"autoCompactWindow"`
			} `json:"modelSettings"`
			Env map[string]string `json:"env"`
		}
		if json.Unmarshal(raw, &settings) != nil {
			return 0
		}
		if settings.AutoCompactWindow != nil {
			compactWindow = min(compactWindow, *settings.AutoCompactWindow)
		}
		for _, key := range []string{model, canonicalModel} {
			if configured := settings.ModelSettings[key].AutoCompactWindow; configured != nil {
				compactWindow = min(compactWindow, *configured)
			}
		}
		for _, name := range []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", "CLAUDE_CODE_DISABLE_1M_CONTEXT", "CLAUDE_CODE_MAX_CONTEXT_TOKENS"} {
			if _, found := settings.Env[name]; found {
				return 0
			}
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--settings") || strings.HasPrefix(arg, "--autocompact") {
			return 0
		}
	}
	if value, found := lastClaudeEnvValue(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW"); found {
		configured, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0
		}
		compactWindow = min(compactWindow, max(int64(100000), min(int64(1000000), configured)))
	}
	percent := int64(95)
	if value, found := lastClaudeEnvValue(env, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"); found {
		configured, err := strconv.ParseInt(value, 10, 64)
		if err != nil || configured < 1 || configured > 100 {
			return 0
		}
		percent = min(percent, configured)
	}
	return max(int64(0), min(compactWindow-33000, compactWindow*percent/100))
}

func codexCompactionTokens(window int64, home, cwd string, args []string) int64 {
	if window <= 0 {
		return 0
	}
	limit := window * 90 / 100
	if home == "" {
		return 0
	}
	for _, path := range []string{filepath.Join(home, "config.toml"), filepath.Join(cwd, ".codex", "config.toml")} {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0
		}
		var config struct {
			Limit    *int64 `toml:"model_auto_compact_token_limit"`
			Profile  string `toml:"profile"`
			Profiles map[string]struct {
				Limit *int64 `toml:"model_auto_compact_token_limit"`
			} `toml:"profiles"`
		}
		if toml.Unmarshal(raw, &config) != nil {
			return 0
		}
		if config.Limit != nil {
			limit = min(limit, *config.Limit)
		}
		for index, arg := range args {
			if (arg == "-p" || arg == "--profile") && index+1 < len(args) {
				config.Profile = args[index+1]
			}
			if strings.HasPrefix(arg, "--profile=") {
				config.Profile = strings.TrimPrefix(arg, "--profile=")
			}
		}
		if config.Profile != "" {
			profile, found := config.Profiles[config.Profile]
			if !found {
				return 0
			}
			if profile.Limit != nil {
				limit = min(limit, *profile.Limit)
			}
		}
	}
	for _, arg := range args {
		if strings.Contains(arg, "model_auto_compact_token_limit") {
			_, value, found := strings.Cut(arg, "=")
			configured, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if !found || err != nil {
				return 0
			}
			limit = min(limit, configured)
		}
	}
	return max(int64(0), limit)
}

func (usage ModelRequestUsage) InputSize() int64 {
	return usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
}

func (usage ModelRequestUsage) Validate() error {
	if usage.Provider == "" || usage.Model == "" || len(usage.Provider) > 100 || len(usage.Model) > 255 {
		return fmt.Errorf("last_model_request requires a provider and model")
	}
	if strings.ContainsRune(usage.Provider, '\x00') || strings.ContainsRune(usage.Model, '\x00') {
		return fmt.Errorf("last_model_request provider and model must not contain NUL")
	}
	for _, value := range []int64{usage.InputTokens, usage.CacheReadTokens, usage.CacheWriteTokens, usage.ContextWindow, usage.CLICompactionTokens} {
		if value < 0 || value > 10000000 {
			return fmt.Errorf("last_model_request token counts must be between 0 and 10000000")
		}
	}
	return nil
}
