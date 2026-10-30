package main

import (
	"strings"
	"testing"
)

func TestGitPushRefusals(t *testing.T) {
	t.Setenv("MULTICA_BROKER_SOCKET", "")
	for name, args := range map[string][]string{
		"force":            {"git", "push", "--force"},
		"force short":      {"git", "push", "-f"},
		"force with lease": {"git", "push", "--force-with-lease"},
		"other remote":     {"git", "push", "https://github.com/x/y.git"},
		"delete refspec":   {"git", "push", "origin", ":main"},
		"no broker":        {"git", "push", "origin", "HEAD"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, f := range []string{"force", "force-with-lease", "f"} {
				_ = gitPushCmd.Flags().Set(f, "false") // cobra keeps flag values between Execute calls
			}
			rootCmd.SetArgs(args)
			err := rootCmd.Execute()
			if err == nil {
				t.Fatal("push was accepted")
			}
			if name == "no broker" && !strings.Contains(err.Error(), "no push broker") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
