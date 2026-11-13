package execenv

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

// HarnessProfileManaged selects the generated per-task config home (CHE-3706).
// Reference table of each harness's isolation knobs:
// docs/cheese-work/harness-isolation-knobs.md.
const HarnessProfileManaged = "managed"

// HarnessProfilePersonal is today's launch: the user's own config home.
const HarnessProfilePersonal = "personal"

// LaunchReceiptFile is written into the env root by every managed adapter.
const LaunchReceiptFile = "launch-receipt.json"

const (
	managedClaudeHomeDir   = "claude-home"
	managedClaudeBriefFile = "runtime-brief.md"
)

// LaunchReceipt records what a managed launch was given and denied.
type LaunchReceipt struct {
	Provider      string   `json:"provider"`
	Profile       string   `json:"profile"`
	EnvKeys       []string `json:"env_keys"`
	FilesSeeded   []string `json:"files_seeded"`
	Skills        []string `json:"skills"`
	McpServers    []string `json:"mcp_servers"`
	ExcludedPaths []string `json:"excluded_paths"`
}

func writeLaunchReceipt(envRoot string, r LaunchReceipt) (string, error) {
	r.Profile = HarnessProfileManaged
	for _, l := range []*[]string{&r.EnvKeys, &r.FilesSeeded, &r.Skills, &r.McpServers, &r.ExcludedPaths} {
		if *l == nil {
			*l = []string{}
		}
		sort.Strings(*l)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(envRoot, LaunchReceiptFile)
	return path, os.WriteFile(path, append(data, '\n'), 0o600)
}

func skillNames(skills []SkillContextForEnv) []string {
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	return names
}

// mcpServerNames lists the server keys of a `{"mcpServers": {...}}` config.
func mcpServerNames(raw json.RawMessage) []string {
	var cfg struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	return names
}

// existingPaths returns dir/name for every name that exists under dir.
func existingPaths(dir string, names ...string) []string {
	var out []string
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// ManagedClaudeHome is the result of PrepareManagedClaudeHome.
type ManagedClaudeHome struct {
	ConfigDir   string // value for CLAUDE_CONFIG_DIR
	BriefPath   string // value for --append-system-prompt-file
	ReceiptPath string
}

// claudePersonalDiscovery are the entries of the user's Claude config dir that
// a managed launch never copies (instructions, hooks/settings, plugins, skills,
// subagents, commands).
var claudePersonalDiscovery = []string{"CLAUDE.md", "settings.json", "plugins", "skills", "agents", "commands"}

// claudeAuthEnvKeys is the allowlist of `env` keys copied from the personal
// settings.json into the managed one: provider route, credential and model
// selection only. Proxy-routed installs (no OAuth login) authenticate solely
// through these, so dropping them makes Claude report "Not logged in".
var claudeAuthEnvKeys = map[string]bool{
	"ANTHROPIC_BASE_URL":             true,
	"ANTHROPIC_AUTH_TOKEN":           true,
	"ANTHROPIC_API_KEY":              true,
	"ANTHROPIC_MODEL":                true,
	"ANTHROPIC_SMALL_FAST_MODEL":     true,
	"ANTHROPIC_DEFAULT_OPUS_MODEL":   true,
	"ANTHROPIC_DEFAULT_SONNET_MODEL": true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":  true,
}

// managedClaudeAuthEnv returns the allowlisted `env` entries of the personal
// settings.json. A missing or unparsable file yields none.
func managedClaudeAuthEnv(sourceConfigDir string) map[string]string {
	raw, err := os.ReadFile(filepath.Join(sourceConfigDir, "settings.json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		Env map[string]any `json:"env"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	kept := map[string]string{}
	for k, v := range cfg.Env {
		if s, ok := v.(string); ok && claudeAuthEnvKeys[k] {
			kept[k] = s
		}
	}
	return kept
}

// PrepareManagedClaudeHome builds the allowlisted CLAUDE_CONFIG_DIR for one
// task: credentials, a minimal settings.json (auto-memory off, no hooks, only
// the allowlisted auth/route `env` keys of the personal one) and
// the runtime brief as a standalone file. Missing credentials are not an error
// (API-key and keychain auth carry them elsewhere).
func PrepareManagedClaudeHome(envRoot, sourceConfigDir, brief string, skills []SkillContextForEnv, mcpConfig json.RawMessage) (ManagedClaudeHome, error) {
	configDir := filepath.Join(envRoot, managedClaudeHomeDir)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return ManagedClaudeHome{}, fmt.Errorf("create managed claude home: %w", err)
	}
	seeded := []string{}
	if creds, err := os.ReadFile(filepath.Join(sourceConfigDir, ".credentials.json")); err == nil {
		if err := os.WriteFile(filepath.Join(configDir, ".credentials.json"), creds, 0o600); err != nil {
			return ManagedClaudeHome{}, fmt.Errorf("seed claude credentials: %w", err)
		}
		seeded = append(seeded, ".credentials.json")
	} else if !os.IsNotExist(err) {
		return ManagedClaudeHome{}, fmt.Errorf("read claude credentials: %w", err)
	}
	authEnv := managedClaudeAuthEnv(sourceConfigDir)
	settings := map[string]any{"autoMemoryEnabled": false}
	envKeys := []string{"CLAUDE_CONFIG_DIR"}
	if len(authEnv) > 0 {
		settings["env"] = authEnv
		for k := range authEnv {
			envKeys = append(envKeys, k)
		}
	}
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return ManagedClaudeHome{}, fmt.Errorf("encode managed claude settings: %w", err)
	}
	files := map[string]string{
		"settings.json": string(settingsJSON),
		".claude.json":  `{"hasCompletedOnboarding": true}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(configDir, name), []byte(content+"\n"), 0o600); err != nil {
			return ManagedClaudeHome{}, fmt.Errorf("write managed claude %s: %w", name, err)
		}
		seeded = append(seeded, name)
	}
	briefPath := filepath.Join(envRoot, managedClaudeBriefFile)
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		return ManagedClaudeHome{}, fmt.Errorf("write managed claude brief: %w", err)
	}
	receiptPath, err := writeLaunchReceipt(envRoot, LaunchReceipt{
		Provider:      "claude",
		EnvKeys:       envKeys,
		FilesSeeded:   seeded,
		Skills:        skillNames(skills),
		McpServers:    mcpServerNames(mcpConfig),
		ExcludedPaths: existingPaths(sourceConfigDir, claudePersonalDiscovery...),
	})
	if err != nil {
		return ManagedClaudeHome{}, fmt.Errorf("write launch receipt: %w", err)
	}
	return ManagedClaudeHome{ConfigDir: configDir, BriefPath: briefPath, ReceiptPath: receiptPath}, nil
}

// prepareManagedCodex finishes a managed CODEX_HOME after
// prepareCodexHomeWithOpts(Managed): only assigned skills are installed,
// $HOME/.agents/skills entries are disabled in config.toml, and a receipt is
// written. The personal ~/.codex/skills link step is deliberately skipped.
func prepareManagedCodex(envRoot, codexHome string, skills []SkillContextForEnv, mcpConfig json.RawMessage, logger *slog.Logger) ([]string, error) {
	skillsDir := filepath.Join(codexHome, "skills")
	if err := os.RemoveAll(skillsDir); err != nil {
		return nil, fmt.Errorf("clear codex skills dir: %w", err)
	}
	preparedSkillNames := resolveSkillSlugs(skills)
	if len(skills) > 0 {
		var err error
		preparedSkillNames, err = writeSkillFilesWithNames(skillsDir, skills, nil)
		if err != nil {
			return nil, err
		}
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user home: %w", err)
	}
	universalDir := filepath.Join(userHome, ".agents", "skills")
	universal, universalCanonical := listUniversalSkills(universalDir)
	if err := ensureCodexDisabledSkillsConfig(filepath.Join(codexHome, "config.toml"), codexHome, universal, skills); err != nil {
		return nil, fmt.Errorf("disable personal codex skills: %w", err)
	}
	if err := appendDisabledCodexSkillPaths(filepath.Join(codexHome, "config.toml"), universalCanonical); err != nil {
		return nil, fmt.Errorf("disable canonical personal codex skills: %w", err)
	}
	sharedHome := resolveSharedCodexHome()
	if _, err := writeLaunchReceipt(envRoot, LaunchReceipt{
		Provider:      "codex",
		EnvKeys:       []string{"CODEX_HOME"},
		FilesSeeded:   existingPaths(codexHome, append(append([]string{}, codexSymlinkedFiles...), "config.toml")...),
		Skills:        skillNames(skills),
		McpServers:    mcpServerNames(mcpConfig),
		ExcludedPaths: append(existingPaths(sharedHome, "instructions.md", "skills", filepath.Join("plugins", "cache")), universalDir),
	}); err != nil {
		return nil, fmt.Errorf("write launch receipt: %w", err)
	}
	if logger != nil {
		logger.Info("execenv: managed codex home prepared", "codex_home", codexHome)
	}
	return preparedSkillNames, nil
}

// managedCodexConfigKeys is the top-level allowlist for a managed
// config.toml: provider, model and credential-store selection only. Everything
// else the personal file can carry (mcp_servers, hooks, plugins, skills.config,
// profiles, notify, instruction-file keys, ...) is dropped.
var managedCodexConfigKeys = map[string]bool{
	"model":                          true,
	"model_provider":                 true,
	"model_providers":                true,
	"model_reasoning_effort":         true,
	"model_reasoning_summary":        true,
	"model_verbosity":                true,
	"model_context_window":           true,
	"model_auto_compact_token_limit": true,
	"model_catalog_json":             true,
	"service_tier":                   true,
	"openai_base_url":                true,
	"chatgpt_base_url":               true,
	"oss_provider":                   true,
	"preferred_auth_method":          true,
	"forced_login_method":            true,
	"forced_chatgpt_workspace_id":    true,
	"cli_auth_credentials_store":     true,
}

// writeManagedCodexConfig replaces dst with the allowlisted subset of the
// personal config at src. A missing src leaves no dst, so a reused home
// previously prepared as personal cannot keep stale personal config.
func writeManagedCodexConfig(src, dst string) error {
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale managed config: %w", err)
	}
	raw, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read personal codex config: %w", err)
	}
	var all map[string]any
	if err := toml.Unmarshal(raw, &all); err != nil {
		return fmt.Errorf("parse personal codex config: %w", err)
	}
	kept := map[string]any{}
	for k, v := range all {
		if managedCodexConfigKeys[k] {
			kept[k] = v
		}
	}
	// Native Windows sandbox opt-in: without it the sandbox policy falls back to
	// danger-full-access. Only the `sandbox` selector is kept from [windows].
	if win, ok := all["windows"].(map[string]any); ok {
		// Any value is copied: an unknown one stays undecidable and fails closed.
		if mode, ok := win["sandbox"]; ok {
			kept["windows"] = map[string]any{"sandbox": mode}
		}
	}
	out, err := toml.Marshal(kept)
	if err != nil {
		return fmt.Errorf("encode managed codex config: %w", err)
	}
	// 0600: model_providers may carry credentials; later rewrites keep the mode.
	return os.WriteFile(dst, out, 0o600)
}

// maxUniversalSkillDepth bounds the walk below ~/.agents/skills.
const maxUniversalSkillDepth = 12

// listUniversalSkills returns, for every SKILL.md under root at any depth, a
// disable key (slash-relative skill dir) and the canonical SKILL.md path.
// Codex discovers skills recursively and may report either the logical or the
// canonical path, so both are disabled. Symlinked root, grouping dirs and skill
// dirs are followed; a dir already on the current ancestor chain is skipped
// (cycle protection) and depth is capped.
func listUniversalSkills(root string) (keys []RuntimeSkillRefForEnv, canonical []string) {
	var walk func(dir, rel string, ancestors map[string]bool, depth int)
	walk = func(dir, rel string, ancestors map[string]bool, depth int) {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || ancestors[real] || depth > maxUniversalSkillDepth {
			return
		}
		ancestors[real] = true
		defer delete(ancestors, real)
		if rel != "" {
			if info, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil && !info.IsDir() {
				keys = append(keys, RuntimeSkillRefForEnv{Root: "universal", Key: rel})
				logical := filepath.Join(dir, "SKILL.md")
				if file, err := filepath.EvalSymlinks(logical); err == nil && file != logical {
					canonical = append(canonical, file)
				}
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			child := filepath.Join(dir, e.Name())
			if info, err := os.Stat(child); err != nil || !info.IsDir() {
				continue
			}
			walk(child, path.Join(rel, e.Name()), ancestors, depth+1)
		}
	}
	walk(root, "", map[string]bool{}, 0)
	return keys, canonical
}
