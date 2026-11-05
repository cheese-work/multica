package execenv

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
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
	if !strings.Contains(string(cfg), `model = "x"`) {
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
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
