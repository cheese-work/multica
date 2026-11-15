package main

import (
	"errors"
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
		"Only the task branch of the task repository is accepted; delete, tags, other refs and a bare --force are refused.\n" +
		"--force-with-lease=<branch>:<sha> may replace the remote task branch when it still points at <sha>.\n" +
		"The `git` wrapper in a restricted run maps `git push` to this command.",
	Args: cobra.MaximumNArgs(2),
	RunE: runGitPush,
}

func init() {
	gitCmd.AddCommand(gitPushCmd)
	gitPushCmd.Flags().BoolP("set-upstream", "u", false, "Accepted and ignored: the broker owns the remote")
	gitPushCmd.Flags().Bool("force", false, "Refused: force needs a lease")
	gitPushCmd.Flags().String("force-with-lease", "", "Replace the remote task branch only if it still points at <sha>: =<branch>:<sha>, or the remote-tracking ref when bare")
	gitPushCmd.Flags().Lookup("force-with-lease").NoOptDefVal = broker.TrackingLease
	gitPushCmd.Flags().BoolP("f", "f", false, "")
	_ = gitPushCmd.Flags().MarkHidden("f")
}

func runGitPush(cmd *cobra.Command, args []string) error {
	for _, f := range []string{"force", "f"} {
		if on, _ := cmd.Flags().GetBool(f); on {
			return errors.New("push --force is refused: use --force-with-lease=<branch>:<sha>")
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	spec, _ := cmd.Flags().GetString("force-with-lease")
	lease, err := broker.Lease(dir, spec)
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
	return broker.PushWithLease(cmd.Context(), sock, dir, lease, cmd.OutOrStdout())
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

var gitReadTokenCmd = &cobra.Command{
	Use:   "read-token",
	Short: "Print the run's read-only GitHub token from the push broker",
	Long: "Used by the `gh` wrapper in a restricted run. The broker holds the token in memory and replaces it when it nears expiry,\n" +
		"revoking the previous one. Nothing is written to disk.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		sock := os.Getenv(broker.SocketEnv)
		if sock == "" {
			return fmt.Errorf("%s is not set: this run has no push broker", broker.SocketEnv)
		}
		tok, err := broker.CurrentReadToken(cmd.Context(), sock)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), tok)
		return err
	},
}

func init() { gitCmd.AddCommand(gitReadTokenCmd) }
