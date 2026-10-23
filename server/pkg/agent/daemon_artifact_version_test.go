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
	stampHelper, err := filepath.Abs("../../../scripts/release-stamp.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{script, filepath.Join(dir, "scripts", "daemon-artifact-version.sh")},
		{stampHelper, filepath.Join(dir, "scripts", "release-stamp.sh")},
	} {
		data, err := os.ReadFile(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		writeTestExecutable(t, pair[1], data)
	}
	if err := os.WriteFile(filepath.Join(dir, "UPSTREAM_VERSION"), []byte("0.6.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_AUTHOR_DATE=2026-10-21T01:56:00Z", "GIT_COMMITTER_DATE=2026-10-21T01:56:00Z")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "a")
	head := git("rev-parse", "HEAD")

	cmd := exec.Command("bash", filepath.Join(dir, "scripts", "daemon-artifact-version.sh"))
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	version := strings.TrimSpace(string(out))
	if version != "0.6.3-20261021-0856" || !regexp.MustCompile(`^\d+\.\d+\.\d+-\d{8}-\d{4}$`).MatchString(version) {
		t.Fatalf("version %q = want 0.6.3-20261021-0856", version)
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
