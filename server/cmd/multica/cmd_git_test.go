package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitPushRefusals(t *testing.T) {
	t.Setenv("MULTICA_BROKER_SOCKET", "")
	for name, args := range map[string][]string{
		"force":                      {"--force"},
		"force short":                {"-f"},
		"bare lease no tracking ref": {"--force-with-lease"},
		"lease other branch":         {"--force-with-lease=main:" + strings.Repeat("a", 40)},
		"other remote":               {"https://github.com/x/y.git"},
		"delete refspec":             {"origin", ":main"},
		"no broker":                  {"origin", "HEAD"},
	} {
		t.Run(name, func(t *testing.T) {
			resetPushFlags()
			// Not rootCmd.Execute: it registers a "help" command on the shared root.
			if err := gitPushCmd.ParseFlags(args); err != nil {
				t.Fatal(err)
			}
			err := runGitPush(gitPushCmd, gitPushCmd.Flags().Args())
			if err == nil {
				t.Fatal("push was accepted")
			}
			if name == "no broker" && !strings.Contains(err.Error(), "no push broker") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// cobra keeps flag values between parses.
func resetPushFlags() {
	for _, f := range []string{"force", "f"} {
		_ = gitPushCmd.Flags().Set(f, "false")
	}
	_ = gitPushCmd.Flags().Set("force-with-lease", "")
}

// A well-formed lease for the checked-out branch passes flag handling and then
// stops where a run without a broker stops.
func TestGitPushLeaseReachesBroker(t *testing.T) {
	t.Setenv("MULTICA_BROKER_SOCKET", "")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/agent/x/t1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	resetPushFlags()
	if err := gitPushCmd.ParseFlags([]string{"--force-with-lease=agent/x/t1:" + strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	if err := runGitPush(gitPushCmd, nil); err == nil || !strings.Contains(err.Error(), "no push broker") {
		t.Fatalf("err = %v, want the no-broker stop", err)
	}
}

func TestGitPRNoBroker(t *testing.T) {
	t.Setenv("MULTICA_BROKER_SOCKET", "")
	if err := runGitPR(gitPRCmd, []string{"create"}); err == nil || !strings.Contains(err.Error(), "no push broker") {
		t.Fatalf("pr without a broker: %v", err)
	}
}

func TestGitReadTokenNoBroker(t *testing.T) {
	t.Setenv("MULTICA_BROKER_SOCKET", "")
	if err := gitReadTokenCmd.RunE(gitReadTokenCmd, nil); err == nil || !strings.Contains(err.Error(), "no push broker") {
		t.Fatalf("read-token without a broker: %v", err)
	}
}
