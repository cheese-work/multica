package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The CI daemon artifact's main.version (scripts/daemon-artifact-version.sh)
// is advertised as the daemon's CLIVersion and must pass the unmodified
// server capability gates; the exact commit is stamped separately.
func TestDaemonArtifactVersionPassesCapabilityGates(t *testing.T) {
	script, err := filepath.Abs("../../../scripts/daemon-artifact-version.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	for i, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", ".")
		git("commit", "-m", name)
		if i == 0 {
			git("tag", "v0.6.0")
		}
	}
	head := git("rev-parse", "HEAD")

	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	version := strings.TrimSpace(string(out))
	if !regexp.MustCompile(`^v0\.6\.0-2-g[0-9a-f]{12}$`).MatchString(version) || !strings.HasPrefix(head, version[strings.Index(version, "-g")+2:]) {
		t.Fatalf("version %q does not describe %s", version, head)
	}

	for _, min := range []string{MinQuickCreateCLIVersion, MinQuickCreateFieldsCLIVersion} {
		if err := CheckMinCLIVersionFor(version, min); err != nil {
			t.Fatalf("CheckMinCLIVersionFor(%q, %q) = %v", version, min, err)
		}
	}
	// Regression pin: the earlier `main-<sha>` stamp is rejected by the same gate.
	if err := CheckMinCLIVersion("main-" + head[:12]); err == nil {
		t.Fatal("gate unexpectedly accepts main-<sha>; the pin no longer proves anything")
	}
}
