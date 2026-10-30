package daemon

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

func putFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func hostHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	putFile(t, filepath.Join(h, ".claude", ".credentials.json"), "claude-secret", 0o600)
	putFile(t, filepath.Join(h, ".codex", "auth.json"), "codex-secret", 0o600)
	putFile(t, filepath.Join(h, ".config", "github-apps", "key.pem"), "app-key", 0o600)
	putFile(t, filepath.Join(h, ".ssh", "id_ed25519"), "ssh-key", 0o600)
	return h
}

func TestRestrictedHomeClaudeCredentialCopy(t *testing.T) {
	user, root := hostHome(t), t.TempDir()
	home, err := prepareRestrictedHome(homeLayout{RootDir: root, UserHome: user, Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if home != filepath.Join(root, "home") {
		t.Fatalf("home = %q", home)
	}
	if fi, err := os.Stat(home); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("home dir: %v %v", fi, err)
	}
	cred := filepath.Join(home, ".claude", ".credentials.json")
	fi, err := os.Lstat(cred)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o400 {
		t.Fatalf("credential copy: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(cred); string(b) != "claude-secret" {
		t.Error("credential copy has wrong bytes")
	}
	for _, rel := range []string{".config/github-apps/key.pem", ".ssh/id_ed25519", ".codex/auth.json"} {
		if _, err := os.Lstat(filepath.Join(home, rel)); err == nil {
			t.Errorf("per-run HOME contains %s", rel)
		}
	}
}

func TestRestrictedHomeCodexReplacesSharedSymlink(t *testing.T) {
	user, root := hostHome(t), t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(user, ".codex", "auth.json")
	if err := os.Symlink(shared, filepath.Join(codexHome, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRestrictedHome(homeLayout{RootDir: root, UserHome: user, Provider: "codex", CodexHome: codexHome}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(codexHome, "auth.json")
	fi, err := os.Lstat(dst)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o400 {
		t.Fatalf("codex auth.json is not a read-only copy: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(shared); string(b) != "codex-secret" {
		t.Error("shared credential changed")
	}
	if _, err := os.Lstat(filepath.Join(root, "home", ".claude")); err == nil {
		t.Error("codex run received the claude credential")
	}
}

func TestRestrictedHomeMissingCredentialIsNotAnError(t *testing.T) {
	home, err := prepareRestrictedHome(homeLayout{RootDir: t.TempDir(), UserHome: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
		t.Error("credential appeared from nothing")
	}
}

func TestRestrictedHomeOtherProviderGetsNoCredential(t *testing.T) {
	home, err := prepareRestrictedHome(homeLayout{RootDir: t.TempDir(), UserHome: hostHome(t), Provider: "gemini"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude")); err == nil {
		t.Error("unrelated provider got a credential")
	}
}

func TestRestrictedHomeTemplateCopiedWithoutSymlinks(t *testing.T) {
	user, tmpl := hostHome(t), t.TempDir()
	putFile(t, filepath.Join(tmpl, ".claude", "settings.json"), "{}", 0o644)
	if err := os.Symlink(filepath.Join(user, ".ssh", "id_ed25519"), filepath.Join(tmpl, "link")); err != nil {
		t.Fatal(err)
	}
	home, err := prepareRestrictedHome(homeLayout{RootDir: t.TempDir(), UserHome: user, Template: tmpl, Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); string(b) != "{}" {
		t.Error("template file not copied")
	}
	if _, err := os.Lstat(filepath.Join(home, "link")); err == nil {
		t.Error("template symlink was followed or copied")
	}
}

func TestRestrictedHomeReuseRefreshesCredential(t *testing.T) {
	user, root := hostHome(t), t.TempDir()
	l := homeLayout{RootDir: root, UserHome: user, Provider: "claude"}
	if _, err := prepareRestrictedHome(l); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(user, ".claude", ".credentials.json"), "rotated", 0o600)
	home, err := prepareRestrictedHome(l)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json")); string(b) != "rotated" {
		t.Error("reused HOME kept the stale credential")
	}
}

func TestDirBytes(t *testing.T) {
	d := t.TempDir()
	putFile(t, filepath.Join(d, "a"), "12345", 0o644)
	putFile(t, filepath.Join(d, "sub", "b"), "123", 0o644)
	if got := dirBytes(d); got < 8 {
		t.Errorf("dirBytes = %d, want at least 8", got)
	}
}

func TestLoadConfig_SandboxMode(t *testing.T) {
	stageFakeAgent(t)
	redirectTestHome(t, t.TempDir())
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	overrides := Overrides{ServerURL: "http://localhost:0", WorkspacesRoot: t.TempDir()}

	t.Setenv("MULTICA_SANDBOX_MODE", "")
	if cfg, err := LoadConfig(overrides); err != nil || cfg.SandboxMode != sandbox.ModeOff {
		t.Fatalf("default mode = %q, %v; want off", cfg.SandboxMode, err)
	}
	t.Setenv("MULTICA_SANDBOX_MODE", "on")
	t.Setenv("MULTICA_SANDBOX_HOME_TEMPLATE", "/srv/template")
	cfg, err := LoadConfig(overrides)
	if err != nil || cfg.SandboxMode != sandbox.ModeOn || cfg.SandboxHomeTemplate != "/srv/template" {
		t.Fatalf("mode on: %q %q %v", cfg.SandboxMode, cfg.SandboxHomeTemplate, err)
	}
	t.Setenv("MULTICA_SANDBOX_MODE", "maybe")
	if _, err := LoadConfig(overrides); err == nil || !strings.Contains(err.Error(), "MULTICA_SANDBOX_MODE") {
		t.Fatalf("invalid mode error = %v, want named validation error", err)
	}
}

func runLayout(t *testing.T) homeLayout {
	return homeLayout{RootDir: t.TempDir(), UserHome: hostHome(t), Provider: "claude"}
}

func TestApplyRestrictedHomeOffLeavesAmbientRun(t *testing.T) {
	l := runLayout(t)
	agentEnv := map[string]string{}
	done, err := applyRestrictedHome(sandbox.ModeOff, l, agentEnv, slog.Default())
	if err != nil || len(agentEnv) != 0 {
		t.Fatalf("off: env=%v err=%v", agentEnv, err)
	}
	done()
	if _, err := os.Stat(filepath.Join(l.RootDir, "home")); err == nil {
		t.Error("mode off created a per-run HOME")
	}
}

func TestApplyRestrictedHomeOnSetsHomeAndLogsSize(t *testing.T) {
	l := runLayout(t)
	var logs bytes.Buffer
	agentEnv := map[string]string{"HOME": "/daemon/home"}
	done, err := applyRestrictedHome(sandbox.ModeOn, l, agentEnv, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if agentEnv["HOME"] != filepath.Join(l.RootDir, "home") {
		t.Fatalf("HOME = %q", agentEnv["HOME"])
	}
	done()
	if !strings.Contains(logs.String(), "home_bytes=") {
		t.Errorf("no M2 measurement in log: %s", logs.String())
	}
	if strings.Contains(logs.String(), "claude-secret") {
		t.Error("log contains the credential value")
	}
}

func TestApplyRestrictedHomeAuditLogsNamesNotValues(t *testing.T) {
	t.Setenv("GH_TOKEN", "super-secret-value")
	var logs bytes.Buffer
	agentEnv := map[string]string{}
	if _, err := applyRestrictedHome(sandbox.ModeAudit, runLayout(t), agentEnv, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if len(agentEnv) != 0 {
		t.Errorf("audit changed the run env: %v", agentEnv)
	}
	if !strings.Contains(logs.String(), "GH_TOKEN") || strings.Contains(logs.String(), "super-secret-value") {
		t.Errorf("audit log must name GH_TOKEN and never its value: %s", logs.String())
	}
}

func TestRestrictedHomeReuseDoesNotFollowParentSymlink(t *testing.T) {
	user, root, outside := hostHome(t), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "home", ".claude")); err != nil {
		t.Fatal(err)
	}
	_, err := prepareRestrictedHome(homeLayout{RootDir: root, UserHome: user, Provider: "claude"})
	if err == nil {
		t.Error("expected an error for a destination parent that escapes the per-run HOME")
	}
	if _, statErr := os.Lstat(filepath.Join(outside, ".credentials.json")); statErr == nil {
		t.Error("credential was written outside the per-run HOME")
	}
}

func TestRestrictedHomeReuseTemplateDoesNotTruncateSymlinkTarget(t *testing.T) {
	user, root, tmpl, outside := hostHome(t), t.TempDir(), t.TempDir(), t.TempDir()
	putFile(t, filepath.Join(tmpl, "settings.json"), "{}", 0o644)
	victim := filepath.Join(outside, "victim")
	putFile(t, victim, "keep-me", 0o600)
	if err := os.MkdirAll(filepath.Join(root, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "home", "settings.json")); err != nil {
		t.Fatal(err)
	}
	_, _ = prepareRestrictedHome(homeLayout{RootDir: root, UserHome: user, Template: tmpl, Provider: "claude"})
	if b, _ := os.ReadFile(victim); string(b) != "keep-me" {
		t.Errorf("symlink target was overwritten: %q", b)
	}
}

func TestRestrictedHomeCodexUsesCustomSharedHome(t *testing.T) {
	user, root, custom := hostHome(t), t.TempDir(), t.TempDir()
	putFile(t, filepath.Join(custom, "auth.json"), "custom-secret", 0o600)
	t.Setenv("CODEX_HOME", custom)
	codexHome := filepath.Join(root, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(custom, "auth.json"), filepath.Join(codexHome, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRestrictedHome(homeLayout{RootDir: root, UserHome: user, Provider: "codex", CodexHome: codexHome}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(codexHome, "auth.json")
	fi, err := os.Lstat(dst)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("auth.json is not a regular copy: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "custom-secret" {
		t.Errorf("copied %q, want the custom CODEX_HOME credential", b)
	}
}

func TestRestrictedHomeCodexMissingSourceLeavesNoCredential(t *testing.T) {
	user, root, custom := hostHome(t), t.TempDir(), t.TempDir() // custom has no auth.json
	t.Setenv("CODEX_HOME", custom)
	codexHome := filepath.Join(root, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(user, ".codex", "auth.json"), filepath.Join(codexHome, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRestrictedHome(homeLayout{RootDir: root, UserHome: user, Provider: "codex", CodexHome: codexHome}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(codexHome, "auth.json")); err == nil {
		t.Error("shared auth symlink or stale copy survived a missing source")
	}
}
