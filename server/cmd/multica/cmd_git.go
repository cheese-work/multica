package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/daemon/broker"
)

var gitCmd = &cobra.Command{
	Use:   "git",
	Short: "Git operations that need a credential the run does not hold",
}

var gitPushCmd = &cobra.Command{
	Use:   "push [origin] [HEAD|<branch>]",
	Short: "Push the task branch through the daemon's push broker",
	Long: "Sends the checked-out task branch to the daemon, which pushes it with a credential this run never holds.\n" +
		"Only the task branch of the task repository is accepted; force, delete, tags and other refs are refused.\n" +
		"The `git` wrapper in a restricted run maps `git push` to this command.",
	Args: cobra.MaximumNArgs(2),
	RunE: runGitPush,
}

func init() {
	gitCmd.AddCommand(gitPushCmd)
	gitPushCmd.Flags().BoolP("set-upstream", "u", false, "Accepted and ignored: the broker owns the remote")
	for _, f := range []string{"force", "force-with-lease"} {
		gitPushCmd.Flags().Bool(f, false, "Refused: the broker never force-pushes")
	}
	gitPushCmd.Flags().BoolP("f", "f", false, "")
	_ = gitPushCmd.Flags().MarkHidden("f")
}

func runGitPush(cmd *cobra.Command, args []string) error {
	for _, f := range []string{"force", "force-with-lease", "f"} {
		if on, _ := cmd.Flags().GetBool(f); on {
			return fmt.Errorf("push --%s is refused: the broker never force-pushes", f)
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := checkPushArgs(args, dir); err != nil {
		return err
	}
	sock := os.Getenv(broker.SocketEnv)
	if sock == "" {
		return fmt.Errorf("%s is not set: this run has no push broker", broker.SocketEnv)
	}
	return broker.Push(cmd.Context(), sock, dir, cmd.OutOrStdout())
}

// checkPushArgs accepts the remote "origin" and the current branch (or HEAD);
// the broker pushes only its pinned ref whatever is asked.
func checkPushArgs(args []string, dir string) error {
	if len(args) > 0 && args[0] != "origin" {
		return fmt.Errorf("push to %q is refused: only origin, through the broker", args[0])
	}
	if len(args) < 2 || args[1] == "HEAD" {
		return nil
	}
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "-q", "--short", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(out)) != args[1] {
		return fmt.Errorf("push %q is refused: only the checked-out task branch may be pushed", args[1])
	}
	return nil
}
