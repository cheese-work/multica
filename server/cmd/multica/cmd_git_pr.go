package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/daemon/broker"
)

var gitPRCmd = &cobra.Command{
	Use:   "pr create|edit|ready|comment",
	Short: "Operate on this run's own pull request through the daemon's broker",
	Long: "Creates a PR from the task branch, edits its title or body, marks it ready, or comments on it.\n" +
		"Only a PR whose head is this run's task branch is accepted. There is no merge and no review approval.",
	Args: cobra.ExactArgs(1),
	RunE: runGitPR,
}

func init() {
	gitCmd.AddCommand(gitPRCmd)
	f := gitPRCmd.Flags()
	f.Int("number", 0, "PR number (edit, ready, comment)")
	f.String("title", "", "PR title (create, edit)")
	f.String("body", "", "PR body (create, edit) or comment text (comment)")
	f.String("base", "", "Base branch (create)")
	f.Bool("draft", false, "Create as a draft")
}

func runGitPR(cmd *cobra.Command, args []string) error {
	sock := os.Getenv(broker.SocketEnv)
	if sock == "" {
		return fmt.Errorf("%s is not set: this run has no push broker", broker.SocketEnv)
	}
	f := cmd.Flags()
	req := broker.PRRequest{Op: args[0]}
	req.Number, _ = f.GetInt("number")
	req.Base, _ = f.GetString("base")
	req.Draft, _ = f.GetBool("draft")
	for name, dst := range map[string]**string{"title": &req.Title, "body": &req.Body} {
		if f.Changed(name) {
			v, _ := f.GetString(name)
			*dst = &v
		}
	}
	res, err := broker.PR(cmd.Context(), sock, req)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "#%d %s\n", res.Number, res.URL)
	return err
}
