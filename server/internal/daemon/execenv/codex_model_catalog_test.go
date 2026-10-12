package execenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeCodexDebugModels writes a fake `codex` executable that ignores its
// arguments and echoes stdout to stdout, mimicking `codex debug models`.
// exitNonZero makes it fail instead, for the fail-open paths.
func writeFakeCodexDebugModels(t *testing.T, dir, stdout string, exitNonZero bool) string {
	t.Helper()
	bin := filepath.Join(dir, "codex")
	exit := "exit 0"
	if exitNonZero {
		exit = "exit 1"
	}
	body := "#!/bin/sh\ncat <<'MULTICA_EOF'\n" + stdout + "\nMULTICA_EOF\n" + exit + "\n"
	writeTestExecutable(t, bin, []byte(body))
	return bin
}

const fakeCatalogJSON = `{"models":[` +
	`{"slug":"gpt-5.6-terra","multi_agent_version":"v2","tool_mode":"code_mode_only","display_name":"Terra"},` +
	`{"slug":"gpt-4.1","multi_agent_version":"v1","tool_mode":"shell","display_name":"GPT 4.1"}` +
	`]}`

func TestEnsureCodexModelCatalogOverrideEscapeHatch(t *testing.T) {
	// Cannot run in parallel: mutates process env.
	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex-home: %v", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	original := "model = \"o3\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	t.Setenv(MulticaCodexMultiAgentEnv, "1")

	bin := writeFakeCodexDebugModels(t, dir, fakeCatalogJSON, false)
	opts := CodexHomeOptions{CodexBinaryPath: bin}
	if err := ensureCodexModelCatalogOverride(codexHome, opts, nil); err != nil {
		t.Fatalf("ensureCodexModelCatalogOverride failed: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	if string(data) != original {
		t.Errorf("expected config.toml untouched when escape hatch set\n--- got ---\n%s\n--- want ---\n%s", data, original)
	}
	if _, err := os.Stat(filepath.Join(codexHome, codexModelCatalogFileName)); !os.IsNotExist(err) {
		t.Errorf("expected no catalog file written when escape hatch set, stat err: %v", err)
	}
}

// TestEnsureCodexModelCatalogOverrideEmptyBinaryPathSkips pins the contract
// that an unset CodexBinaryPath skips the override outright rather than
// falling back to an exec.LookPath("codex") PATH lookup. Dozens of other
// package tests build CodexHomeOptions without ever intending to exercise
// this override (they test skills/sandbox/session behavior); a PATH fallback
// would make all of them silently execute whatever `codex` happens to be
// installed on the machine running the suite, which AGENTS.md forbids for
// default tests.
func TestEnsureCodexModelCatalogOverrideEmptyBinaryPathSkips(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex-home: %v", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	original := "model = \"o3\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if err := ensureCodexModelCatalogOverride(codexHome, CodexHomeOptions{}, nil); err != nil {
		t.Fatalf("ensureCodexModelCatalogOverride must fail open, got error: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	if string(data) != original {
		t.Errorf("expected config.toml untouched with empty CodexBinaryPath\n--- got ---\n%s\n--- want ---\n%s", data, original)
	}
	if _, err := os.Stat(filepath.Join(codexHome, codexModelCatalogFileName)); !os.IsNotExist(err) {
		t.Errorf("expected no catalog file written with empty CodexBinaryPath, stat err: %v", err)
	}
}

func TestEnsureCodexModelCatalogOverrideRewritesCatalog(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex-home: %v", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	original := "model = \"o3\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	bin := writeFakeCodexDebugModels(t, dir, fakeCatalogJSON, false)
	opts := CodexHomeOptions{CodexBinaryPath: bin}
	if err := ensureCodexModelCatalogOverride(codexHome, opts, nil); err != nil {
		t.Fatalf("ensureCodexModelCatalogOverride failed: %v", err)
	}

	catalogPath := filepath.Join(codexHome, codexModelCatalogFileName)
	catalogData, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read rewritten catalog: %v", err)
	}
	var cat map[string]any
	if err := json.Unmarshal(catalogData, &cat); err != nil {
		t.Fatalf("rewritten catalog is not valid JSON: %v\n%s", err, catalogData)
	}
	models, ok := cat["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("expected 2 models in rewritten catalog, got: %#v", cat["models"])
	}
	byModel := map[string]map[string]any{}
	for _, m := range models {
		entry := m.(map[string]any)
		byModel[entry["slug"].(string)] = entry
	}

	terra := byModel["gpt-5.6-terra"]
	if _, ok := terra["multi_agent_version"]; ok {
		t.Errorf("expected multi_agent_version cleared for gpt-5.6-terra, got: %#v", terra)
	}
	if _, ok := terra["tool_mode"]; ok {
		t.Errorf("expected tool_mode cleared for gpt-5.6-terra, got: %#v", terra)
	}
	if terra["display_name"] != "Terra" {
		t.Errorf("expected unrelated field display_name preserved, got: %#v", terra)
	}

	gpt41 := byModel["gpt-4.1"]
	if _, ok := gpt41["multi_agent_version"]; ok {
		t.Errorf("expected multi_agent_version cleared for gpt-4.1, got: %#v", gpt41)
	}
	if got, ok := gpt41["tool_mode"]; !ok || got != "shell" {
		t.Errorf("expected tool_mode left untouched for non-gpt-5.6 model, got: %#v", gpt41)
	}

	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	got := string(configData)
	if !strings.Contains(got, multicaModelCatalogBeginMarker) {
		t.Errorf("expected managed model-catalog block, got:\n%s", got)
	}
	wantRef := "model_catalog_json = " + strconvQuote(filepath.ToSlash(catalogPath))
	if !strings.Contains(got, wantRef) {
		t.Errorf("expected config.toml to reference rewritten catalog path\ngot:\n%s\nwant substring: %s", got, wantRef)
	}
	if !strings.Contains(got, "model = \"o3\"") {
		t.Errorf("expected user's model setting preserved, got:\n%s", got)
	}

	parsed := parseTOML(t, got)
	if parsed["model_catalog_json"] != catalogPath {
		t.Errorf("expected parsed model_catalog_json == %q, got: %#v", catalogPath, parsed["model_catalog_json"])
	}
}

// strconvQuote mirrors Go's %q formatting for a string, matching how
// renderModelCatalogBlock writes the TOML value, without importing strconv
// twice under a different name in this file.
func strconvQuote(s string) string {
	quoted, _ := json.Marshal(s)
	return string(quoted)
}

func TestEnsureCodexModelCatalogOverrideBinaryNotFound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex-home: %v", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	original := "model = \"o3\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// A binary path that does not exist on disk: exec.CommandContext.Run
	// fails at exec, not merely at exit — this exercises the "binary not
	// found" fail-open path distinctly from a non-zero exit.
	opts := CodexHomeOptions{CodexBinaryPath: filepath.Join(dir, "no-such-codex-binary")}
	if err := ensureCodexModelCatalogOverride(codexHome, opts, nil); err != nil {
		t.Fatalf("ensureCodexModelCatalogOverride must fail open, got error: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	if string(data) != original {
		t.Errorf("expected config.toml untouched when codex binary is missing\n--- got ---\n%s\n--- want ---\n%s", data, original)
	}
	if _, err := os.Stat(filepath.Join(codexHome, codexModelCatalogFileName)); !os.IsNotExist(err) {
		t.Errorf("expected no catalog file written when codex binary is missing, stat err: %v", err)
	}
}

func TestEnsureCodexModelCatalogOverrideNonZeroExit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex-home: %v", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	original := "model = \"o3\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	bin := writeFakeCodexDebugModels(t, dir, "", true)
	opts := CodexHomeOptions{CodexBinaryPath: bin}
	if err := ensureCodexModelCatalogOverride(codexHome, opts, nil); err != nil {
		t.Fatalf("ensureCodexModelCatalogOverride must fail open, got error: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	if string(data) != original {
		t.Errorf("expected config.toml untouched on non-zero exit\n--- got ---\n%s\n--- want ---\n%s", data, original)
	}
	if _, err := os.Stat(filepath.Join(codexHome, codexModelCatalogFileName)); !os.IsNotExist(err) {
		t.Errorf("expected no catalog file written on non-zero exit, stat err: %v", err)
	}
}

func TestEnsureCodexModelCatalogOverrideInvalidJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex-home: %v", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	original := "model = \"o3\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	bin := writeFakeCodexDebugModels(t, dir, "not json at all", false)
	opts := CodexHomeOptions{CodexBinaryPath: bin}
	if err := ensureCodexModelCatalogOverride(codexHome, opts, nil); err != nil {
		t.Fatalf("ensureCodexModelCatalogOverride must fail open, got error: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	if string(data) != original {
		t.Errorf("expected config.toml untouched on invalid JSON\n--- got ---\n%s\n--- want ---\n%s", data, original)
	}
	if _, err := os.Stat(filepath.Join(codexHome, codexModelCatalogFileName)); !os.IsNotExist(err) {
		t.Errorf("expected no catalog file written on invalid JSON, stat err: %v", err)
	}
}

func TestEnsureCodexModelCatalogOverrideIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex-home: %v", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	original := "model = \"o3\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	bin := writeFakeCodexDebugModels(t, dir, fakeCatalogJSON, false)
	opts := CodexHomeOptions{CodexBinaryPath: bin}

	if err := ensureCodexModelCatalogOverride(codexHome, opts, nil); err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	first, _ := os.ReadFile(configPath)

	if err := ensureCodexModelCatalogOverride(codexHome, opts, nil); err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	second, _ := os.ReadFile(configPath)

	if string(first) != string(second) {
		t.Errorf("expected idempotent rewrite\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	parseTOML(t, string(second))
}

func TestEnsureCodexModelCatalogOverrideReplacesUserSetting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex-home: %v", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	original := "model = \"o3\"\n" +
		"model_catalog_json = \"/home/user/my-own-catalog.json\"\n" +
		"\n[profiles.default]\nmodel = \"o3\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	bin := writeFakeCodexDebugModels(t, dir, fakeCatalogJSON, false)
	opts := CodexHomeOptions{CodexBinaryPath: bin}
	if err := ensureCodexModelCatalogOverride(codexHome, opts, nil); err != nil {
		t.Fatalf("ensureCodexModelCatalogOverride failed: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	got := string(data)
	if strings.Contains(got, "/home/user/my-own-catalog.json") {
		t.Errorf("expected user's model_catalog_json to be stripped, got:\n%s", got)
	}
	if !strings.Contains(got, "[profiles.default]") {
		t.Errorf("expected unrelated table preserved, got:\n%s", got)
	}
	if strings.Count(got, "model_catalog_json") != 1 {
		t.Errorf("expected exactly one model_catalog_json key after replacement, got:\n%s", got)
	}

	parsed := parseTOML(t, got)
	catalogPath := filepath.Join(codexHome, codexModelCatalogFileName)
	if parsed["model_catalog_json"] != catalogPath {
		t.Errorf("expected managed catalog path to win, got: %#v", parsed["model_catalog_json"])
	}
}

func TestEnsureCodexModelCatalogOverrideCoexistsWithMultiAgentBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex-home: %v", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	original := "model = \"o3\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if err := ensureCodexMultiAgentConfig(configPath, nil); err != nil {
		t.Fatalf("ensureCodexMultiAgentConfig failed: %v", err)
	}

	bin := writeFakeCodexDebugModels(t, dir, fakeCatalogJSON, false)
	opts := CodexHomeOptions{CodexBinaryPath: bin}
	if err := ensureCodexModelCatalogOverride(codexHome, opts, nil); err != nil {
		t.Fatalf("ensureCodexModelCatalogOverride failed: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	got := string(data)
	parsed := parseTOML(t, got)
	requireMultiAgentDisabled(t, parsed)
	catalogPath := filepath.Join(codexHome, codexModelCatalogFileName)
	if parsed["model_catalog_json"] != catalogPath {
		t.Errorf("expected model_catalog_json set alongside multi-agent block, got: %#v", parsed["model_catalog_json"])
	}
}
