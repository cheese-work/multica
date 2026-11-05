package execenv

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func readReceipt(t *testing.T, envRoot string) LaunchReceipt {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(envRoot, LaunchReceiptFile))
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	var r LaunchReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("parse receipt: %v", err)
	}
	return r
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestPrepareManagedClaudeHome(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, ".credentials.json"), `{"token":"secret"}`)
	writeFile(t, filepath.Join(src, "CLAUDE.md"), "personal instructions")
	writeFile(t, filepath.Join(src, "settings.json"), `{"hooks":{"SessionStart":[]}}`)
	writeFile(t, filepath.Join(src, "plugins", "p", "plugin.json"), "{}")
	writeFile(t, filepath.Join(src, "skills", "mine", "SKILL.md"), "x")
	envRoot := t.TempDir()

	home, err := PrepareManagedClaudeHome(envRoot, src, "the brief",
		[]SkillContextForEnv{{Name: "assigned-skill"}},
		json.RawMessage(`{"mcpServers":{"zeta":{},"alpha":{}}}`))
	if err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(filepath.Join(home.ConfigDir, ".credentials.json")); string(got) != `{"token":"secret"}` {
		t.Errorf("credentials not seeded, got %q", got)
	}
	if fi, err := os.Stat(filepath.Join(home.ConfigDir, ".credentials.json")); err != nil {
		t.Errorf("credentials stat: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials perms = %v, want 0600", fi.Mode().Perm())
	}
	var settings map[string]any
	raw, _ := os.ReadFile(filepath.Join(home.ConfigDir, "settings.json"))
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings.json: %v", err)
	}
	if settings["autoMemoryEnabled"] != false {
		t.Errorf("autoMemoryEnabled = %v, want false", settings["autoMemoryEnabled"])
	}
	if _, ok := settings["hooks"]; ok {
		t.Errorf("personal hooks leaked into managed settings")
	}
	for _, name := range []string{"CLAUDE.md", "plugins", "skills"} {
		if _, err := os.Stat(filepath.Join(home.ConfigDir, name)); err == nil {
			t.Errorf("%s copied into managed home", name)
		}
	}
	if got, _ := os.ReadFile(home.BriefPath); string(got) != "the brief" {
		t.Errorf("brief file = %q", got)
	}

	r := readReceipt(t, envRoot)
	if r.Provider != "claude" || r.Profile != "managed" {
		t.Errorf("receipt identity = %s/%s", r.Provider, r.Profile)
	}
	if !contains(r.EnvKeys, "CLAUDE_CONFIG_DIR") {
		t.Errorf("env keys = %v, want CLAUDE_CONFIG_DIR", r.EnvKeys)
	}
	if !contains(r.FilesSeeded, ".credentials.json") || !contains(r.FilesSeeded, "settings.json") {
		t.Errorf("files seeded = %v", r.FilesSeeded)
	}
	if !contains(r.Skills, "assigned-skill") {
		t.Errorf("skills = %v", r.Skills)
	}
	if strings.Join(r.McpServers, ",") != "alpha,zeta" {
		t.Errorf("mcp servers = %v, want sorted [alpha zeta]", r.McpServers)
	}
	for _, name := range []string{"CLAUDE.md", "settings.json", "plugins", "skills"} {
		if !contains(r.ExcludedPaths, filepath.Join(src, name)) {
			t.Errorf("excluded paths %v missing %s", r.ExcludedPaths, name)
		}
	}
}

func TestPrepareManagedClaudeHome_NoCredentialsIsNotAnError(t *testing.T) {
	src := t.TempDir() // API-key / keychain auth: no credentials file
	envRoot := t.TempDir()
	home, err := PrepareManagedClaudeHome(envRoot, src, "b", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home.ConfigDir, ".credentials.json")); err == nil {
		t.Error("credentials created from nothing")
	}
	if r := readReceipt(t, envRoot); contains(r.FilesSeeded, ".credentials.json") {
		t.Errorf("receipt claims credentials seeded: %v", r.FilesSeeded)
	}
}

func TestPrepareManagedCodex_ExcludesPersonalDiscovery(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	shared := filepath.Join(userHome, ".codex")
	t.Setenv("CODEX_HOME", shared)
	writeFile(t, filepath.Join(shared, "auth.json"), `{"a":1}`)
	writeFile(t, filepath.Join(shared, "config.toml"), "model = \"x\"\n")
	writeFile(t, filepath.Join(shared, "instructions.md"), "personal")
	writeFile(t, filepath.Join(shared, "skills", "personal", "SKILL.md"), "---\nname: personal\ndescription: d\n---\n")
	universal := filepath.Join(userHome, ".agents", "skills", "uni", "SKILL.md")
	writeFile(t, universal, "---\nname: uni\ndescription: d\n---\n")

	envRoot := t.TempDir()
	codexHome := filepath.Join(envRoot, "codex-home")
	logger := discardLogger()
	if err := prepareCodexHomeWithOpts(codexHome, CodexHomeOptions{GOOS: "linux", Managed: true}, logger); err != nil {
		t.Fatal(err)
	}
	ws := []SkillContextForEnv{{Name: "ws", Description: "d", Content: "body"}}
	if err := prepareManagedCodex(envRoot, codexHome, ws, json.RawMessage(`{"mcpServers":{"m1":{}}}`), logger); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(codexHome, "instructions.md")); err == nil {
		t.Error("personal instructions.md copied")
	}
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); err != nil {
		t.Errorf("credentials missing: %v", err)
	}
	cfg, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if !strings.Contains(string(cfg), `model = 'x'`) {
		t.Error("provider config not seeded")
	}
	if !strings.Contains(string(cfg), filepath.ToSlash(universal)) || !strings.Contains(string(cfg), "enabled = false") {
		t.Errorf("$HOME/.agents/skills/uni not disabled in config.toml:\n%s", cfg)
	}
	if _, err := os.Lstat(filepath.Join(codexHome, "skills", "personal")); err == nil {
		t.Error("personal ~/.codex/skills linked into managed home")
	}
	if _, err := os.Stat(filepath.Join(codexHome, "skills", "ws", "SKILL.md")); err != nil {
		t.Errorf("assigned skill missing: %v", err)
	}

	r := readReceipt(t, envRoot)
	if r.Provider != "codex" || r.Profile != "managed" {
		t.Errorf("receipt identity = %s/%s", r.Provider, r.Profile)
	}
	if !contains(r.EnvKeys, "CODEX_HOME") || !contains(r.Skills, "ws") || !contains(r.McpServers, "m1") {
		t.Errorf("receipt = %+v", r)
	}
	if !contains(r.ExcludedPaths, filepath.Join(userHome, ".agents", "skills")) {
		t.Errorf("excluded = %v, want $HOME/.agents/skills", r.ExcludedPaths)
	}
}

func TestPersonalCodexHomeUnchanged(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	shared := filepath.Join(userHome, ".codex")
	t.Setenv("CODEX_HOME", shared)
	writeFile(t, filepath.Join(shared, "instructions.md"), "personal")
	writeFile(t, filepath.Join(shared, "skills", "personal", "SKILL.md"), "---\nname: personal\ndescription: d\n---\n")
	writeFile(t, filepath.Join(userHome, ".agents", "skills", "uni", "SKILL.md"), "x")

	codexHome := filepath.Join(t.TempDir(), "codex-home")
	logger := discardLogger()
	if err := prepareCodexHomeWithOpts(codexHome, CodexHomeOptions{GOOS: "linux"}, logger); err != nil {
		t.Fatal(err)
	}
	if err := hydrateCodexSkills(codexHome, nil, nil, logger); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(codexHome, "instructions.md")); string(got) != "personal" {
		t.Error("personal instructions.md no longer copied")
	}
	if _, err := os.Lstat(filepath.Join(codexHome, "skills", "personal")); err != nil {
		t.Errorf("personal skill no longer linked: %v", err)
	}
	cfg, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if strings.Contains(string(cfg), "skills.config") {
		t.Errorf("personal profile disabled skills:\n%s", cfg)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(codexHome), LaunchReceiptFile)); err == nil {
		t.Error("personal profile wrote a launch receipt")
	}
}

const personalCodexConfig = `model = "gpt-x"
model_provider = "corp"
model_reasoning_effort = "high"
model_instructions_file = "personal-instructions.md"
experimental_instructions_file = "legacy-instructions.md"
notify = ["/home/me/notify.sh"]
profile = "mine"

[model_providers.corp]
name = "Corp"
base_url = "https://llm.example.com/v1"
env_key = "CORP_KEY"

[mcp_servers.personal-mcp]
command = "personal-mcp"

[plugins."p@market"]
enabled = true

[profiles.mine]
model = "other"

[hooks]
on_start = "personal-hook"
`

// A managed Codex home carries provider config only: no personal MCP servers,
// instruction files, hooks, plugins, profiles or notify command, even when the
// home was first prepared as personal and is reused (CHE-3706 review).
func TestPrepareManagedCodex_AllowlistsConfig(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	shared := filepath.Join(userHome, ".codex")
	t.Setenv("CODEX_HOME", shared)
	writeFile(t, filepath.Join(shared, "auth.json"), `{"a":1}`)
	writeFile(t, filepath.Join(shared, "config.toml"), personalCodexConfig)
	writeFile(t, filepath.Join(shared, "personal-instructions.md"), "personal system prompt")
	writeFile(t, filepath.Join(shared, "legacy-instructions.md"), "legacy system prompt")

	envRoot := t.TempDir()
	codexHome := filepath.Join(envRoot, "codex-home")
	logger := discardLogger()
	// Reuse case: the home already exists from a personal launch.
	if err := prepareCodexHomeWithOpts(codexHome, CodexHomeOptions{GOOS: "linux"}, logger); err != nil {
		t.Fatal(err)
	}
	if err := prepareCodexHomeWithOpts(codexHome, CodexHomeOptions{GOOS: "linux", Managed: true}, logger); err != nil {
		t.Fatal(err)
	}
	if err := prepareManagedCodex(envRoot, codexHome, nil, nil, logger); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(raw)
	var parsed struct {
		Model          string `toml:"model"`
		ModelProvider  string `toml:"model_provider"`
		ModelProviders map[string]struct {
			BaseURL string `toml:"base_url"`
			EnvKey  string `toml:"env_key"`
		} `toml:"model_providers"`
	}
	if err := toml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("managed config.toml does not parse: %v\n%s", err, cfg)
	}
	corp := parsed.ModelProviders["corp"]
	if parsed.Model != "gpt-x" || parsed.ModelProvider != "corp" || corp.BaseURL != "https://llm.example.com/v1" || corp.EnvKey != "CORP_KEY" {
		t.Errorf("provider config not kept: %+v\n%s", parsed, cfg)
	}
	for _, drop := range []string{"personal-mcp", "model_instructions_file", "experimental_instructions_file", "notify", "p@market", "profiles", "personal-hook", `profile =`} {
		if strings.Contains(cfg, drop) {
			t.Errorf("personal config %q survived in managed config.toml:\n%s", drop, cfg)
		}
	}
	for _, name := range []string{"instructions.md", "config.json"} {
		if _, err := os.Stat(filepath.Join(codexHome, name)); err == nil {
			t.Errorf("personal file %s present in managed home", name)
		}
	}
}

// Regression: a personal Codex launch keeps the whole personal config and the
// instruction files it references, exactly as before the profile existed.
// Passes before and after CHE-3706 by design.
func TestPrepareCodexHome_PersonalKeepsPersonalConfigVerbatim(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	shared := filepath.Join(userHome, ".codex")
	t.Setenv("CODEX_HOME", shared)
	writeFile(t, filepath.Join(shared, "config.toml"), personalCodexConfig)
	writeFile(t, filepath.Join(shared, "personal-instructions.md"), "personal system prompt")
	writeFile(t, filepath.Join(shared, "legacy-instructions.md"), "legacy system prompt")
	writeFile(t, filepath.Join(shared, "instructions.md"), "personal")

	codexHome := filepath.Join(t.TempDir(), "codex-home")
	logger := discardLogger()
	if err := prepareCodexHomeWithOpts(codexHome, CodexHomeOptions{GOOS: "linux"}, logger); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// Only the sandbox-managed blocks are daemon-added; the personal bytes follow verbatim.
	if !strings.HasSuffix(string(got), personalCodexConfig) {
		t.Errorf("personal config.toml bytes changed:\n%s", got)
	}
	for _, name := range []string{"personal-instructions.md", "legacy-instructions.md", "instructions.md"} {
		if _, err := os.Stat(filepath.Join(codexHome, name)); err != nil {
			t.Errorf("personal file %s no longer copied: %v", name, err)
		}
	}
}

// The managed config.toml can hold provider credentials (inline bearer token),
// so it must never be group- or world-readable, including after the later
// sandbox/skill-strip rewrites (CHE-3706 review).
func TestPrepareManagedCodex_ConfigIsOwnerOnly(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	shared := filepath.Join(userHome, ".codex")
	t.Setenv("CODEX_HOME", shared)
	writeFile(t, filepath.Join(shared, "config.toml"), `model_provider = "corp"

[model_providers.corp]
base_url = "https://llm.example.com/v1"
experimental_bearer_token = "sk-secret-token"
`)
	envRoot := t.TempDir()
	codexHome := filepath.Join(envRoot, "codex-home")
	logger := discardLogger()
	// Personal first (copies the file 0644), then managed on the reused home.
	for _, managed := range []bool{false, true} {
		if err := prepareCodexHomeWithOpts(codexHome, CodexHomeOptions{GOOS: "linux", Managed: managed}, logger); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(codexHome, "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "sk-secret-token") {
		t.Fatalf("provider credential not kept (err=%v):\n%s", err, raw)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("managed config.toml mode = %o, want 600", perm)
	}
}

// A home first prepared as personal links the shared plugin cache. Switching
// the same home to managed must remove that link, since the receipt lists the
// path as excluded (CHE-3706 review).
func TestPrepareManagedCodex_RemovesStalePluginCacheLink(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	shared := filepath.Join(userHome, ".codex")
	t.Setenv("CODEX_HOME", shared)
	writeFile(t, filepath.Join(shared, "config.toml"), "model = \"x\"\n")
	writeFile(t, filepath.Join(shared, "plugins", "cache", "p", "plugin.json"), "{}")
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	logger := discardLogger()
	if err := prepareCodexHomeWithOpts(codexHome, CodexHomeOptions{GOOS: "linux"}, logger); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(codexHome, "plugins", "cache")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("personal launch did not link the plugin cache: %v", err)
	}
	if err := prepareCodexHomeWithOpts(codexHome, CodexHomeOptions{GOOS: "linux", Managed: true}, logger); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Error("shared plugin cache link survived in managed home")
	}
	if _, err := os.Stat(filepath.Join(shared, "plugins", "cache", "p", "plugin.json")); err != nil {
		t.Errorf("removing the link touched the shared cache: %v", err)
	}
}

// Golden captured from origin/main 86b9d79 (before the runtime-profile
// change): the personal Codex home tree and config.toml. Passes on base and head.
func TestPersonalCodexHomeGolden(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	shared := filepath.Join(userHome, ".codex")
	t.Setenv("CODEX_HOME", shared)
	writeFile(t, filepath.Join(shared, "instructions.md"), "personal")
	writeFile(t, filepath.Join(shared, "config.json"), "{}")
	writeFile(t, filepath.Join(shared, "auth.json"), "{}")
	writeFile(t, filepath.Join(shared, "config.toml"), "model = \"gpt-x\"\nmodel_provider = \"corp\"\nnotify = [\"/bin/n\"]\n\n[mcp_servers.personal-mcp]\ncommand = \"x\"\n")
	writeFile(t, filepath.Join(shared, "skills", "personal", "SKILL.md"), "---\nname: personal\ndescription: d\n---\n")
	writeFile(t, filepath.Join(shared, "plugins", "cache", "p", "plugin.json"), "{}")
	writeFile(t, filepath.Join(userHome, ".agents", "skills", "uni", "SKILL.md"), "x")

	codexHome := filepath.Join(t.TempDir(), "codex-home")
	logger := discardLogger()
	if err := prepareCodexHomeWithOpts(codexHome, CodexHomeOptions{GOOS: "linux"}, logger); err != nil {
		t.Fatal(err)
	}
	if err := hydrateCodexSkills(codexHome, nil, nil, logger); err != nil {
		t.Fatal(err)
	}
	var tree strings.Builder
	_ = filepath.WalkDir(codexHome, func(p string, d fs.DirEntry, _ error) error {
		rel, _ := filepath.Rel(codexHome, p)
		if rel == "." {
			return nil
		}
		kind := "f"
		if d.Type()&fs.ModeSymlink != 0 {
			kind = "l"
		} else if d.IsDir() {
			kind = "d"
		}
		tree.WriteString(kind + " " + rel + "\n")
		return nil
	})
	wantTree := "f .models_cache_config.sha256\nl auth.json\nf config.json\nf config.toml\nf instructions.md\nd plugins\nl plugins/cache\nd sessions\nd skills\nl skills/personal\n"
	if tree.String() != wantTree {
		t.Errorf("personal codex home tree drifted\n got:\n%s\nwant:\n%s", tree.String(), wantTree)
	}
	const wantCfg = "# BEGIN multica-managed memory-feature (do not edit; regenerated by daemon)\nfeatures.memories = false\n# END multica-managed memory-feature\n# BEGIN multica-managed memory-config (do not edit; regenerated by daemon)\nmemories.generate_memories = false\nmemories.use_memories = false\n# END multica-managed memory-config\n\n# BEGIN multica-managed multi-agent (do not edit; regenerated by daemon)\nfeatures.multi_agent = false\n# END multica-managed multi-agent\n\n# BEGIN multica-managed (do not edit; regenerated by daemon)\nsandbox_mode = \"danger-full-access\"\n# END multica-managed\n\nmodel = \"gpt-x\"\nmodel_provider = \"corp\"\nnotify = [\"/bin/n\"]\n\n[mcp_servers.personal-mcp]\ncommand = \"x\"\n"
	cfg, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if string(cfg) != wantCfg {
		t.Errorf("personal codex config.toml drifted\n got:\n%s\nwant:\n%s", cfg, wantCfg)
	}
}
