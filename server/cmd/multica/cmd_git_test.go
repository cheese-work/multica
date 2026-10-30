package main

import (
	"strings"
	"testing"
)

func TestGitPushRefusals(t *testing.T) {
	t.Setenv("MULTICA_BROKER_SOCKET", "")
	for name, args := range map[string][]string{
		"force":            {"--force"},
		"force short":      {"-f"},
		"force with lease": {"--force-with-lease"},
		"other remote":     {"https://github.com/x/y.git"},
		"delete refspec":   {"origin", ":main"},
		"no broker":        {"origin", "HEAD"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, f := range []string{"force", "force-with-lease", "f"} {
				_ = gitPushCmd.Flags().Set(f, "false") // cobra keeps flag values between parses
			}
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
