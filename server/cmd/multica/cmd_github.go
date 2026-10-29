package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var githubCmd = &cobra.Command{
	Use:   "github",
	Short: "GitHub integration controls",
}

var githubKillSwitchCmd = &cobra.Command{
	Use:   "kill-switch",
	Short: "Stop or resume runs that GitHub events start (PR merge, checks, review, conflict)",
	Long: "The switch is per workspace and per scope: '*' (everything), an owner, or owner/repo.\n" +
		"`on` drops new GitHub-triggered wakeups for the scope, cancels live GitHub-triggered runs there and revokes\n" +
		"their run tokens. Events that arrive while it is on are dropped, not queued. `off` needs a human login;\n" +
		"an agent's task token can turn it on but never off. `status` only reads.",
}

var githubKillSwitchStatusCmd = &cobra.Command{Use: "status", Short: "Show the active halts (read-only)", Args: exactArgs(0), RunE: runGitHubKillSwitchStatus}
var githubKillSwitchOnCmd = &cobra.Command{Use: "on", Short: "Halt GitHub-triggered dispatch for a scope", Args: exactArgs(0), RunE: runGitHubKillSwitch(true)}
var githubKillSwitchOffCmd = &cobra.Command{Use: "off", Short: "Resume GitHub-triggered dispatch for a scope", Args: exactArgs(0), RunE: runGitHubKillSwitch(false)}

func init() {
	githubCmd.AddCommand(githubKillSwitchCmd)
	githubKillSwitchCmd.AddCommand(githubKillSwitchStatusCmd, githubKillSwitchOnCmd, githubKillSwitchOffCmd)
	githubKillSwitchStatusCmd.Flags().String("output", "table", "Output format: table or json")
	for _, c := range []*cobra.Command{githubKillSwitchOnCmd, githubKillSwitchOffCmd} {
		c.Flags().String("scope", "*", "'*' (org-wide), an owner, or owner/repo")
		c.Flags().String("output", "table", "Output format: table or json")
	}
}

type githubHalts struct {
	Halts []struct {
		Scope    string  `json:"scope"`
		HaltedBy *string `json:"halted_by"`
		HaltedAt string  `json:"halted_at"`
	} `json:"halts"`
	RunsRevoked int `json:"runs_revoked"`
}

func runGitHubKillSwitchStatus(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var resp githubHalts
	if err := client.GetJSON(ctx, "/api/github/dispatch-halt", &resp); err != nil {
		return fmt.Errorf("read kill switch: %w", err)
	}
	return printGitHubHalts(cmd, resp)
}

func runGitHubKillSwitch(halted bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		client, err := newAPIClient(cmd)
		if err != nil {
			return err
		}
		scope, _ := cmd.Flags().GetString("scope")
		ctx, cancel := cli.APIContext(context.Background())
		defer cancel()
		var resp githubHalts
		if err := client.PutJSON(ctx, "/api/github/dispatch-halt", map[string]any{"scope": scope, "halted": halted}, &resp); err != nil {
			return fmt.Errorf("set kill switch: %w", err)
		}
		if halted && resp.RunsRevoked > 0 {
			fmt.Fprintf(os.Stderr, "cancelled %d GitHub-triggered run(s) and revoked their tokens\n", resp.RunsRevoked)
		}
		return printGitHubHalts(cmd, resp)
	}
}

func printGitHubHalts(cmd *cobra.Command, resp githubHalts) error {
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, resp)
	}
	if len(resp.Halts) == 0 {
		fmt.Println("kill switch: off (GitHub-triggered dispatch is running)")
		return nil
	}
	rows := make([][]string, 0, len(resp.Halts))
	for _, h := range resp.Halts {
		by := ""
		if h.HaltedBy != nil {
			by = strings.Split(*h.HaltedBy, "-")[0]
		}
		rows = append(rows, []string{h.Scope, by, relativeTimestamp(h.HaltedAt)})
	}
	cli.PrintTable(os.Stdout, []string{"HALTED_SCOPE", "BY", "SINCE"}, rows)
	return nil
}
