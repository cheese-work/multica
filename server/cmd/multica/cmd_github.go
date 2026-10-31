package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
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

var githubIntakeCmd = &cobra.Command{Use: "intake", Short: "Inspect GitHub webhook intake (read-only)"}

var githubIntakeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List webhook deliveries received since a time, with their dispatch outcome (read-only)",
	Long: "Shows event, delivery GUID, intake state and reason, revision state and reason, and how many runs the delivery started.\n" +
		"No payload or signature is returned. A delivery that is notify-only shows 0 runs.",
	Args: exactArgs(0),
	RunE: runGitHubIntakeList,
}

func init() {
	githubCmd.AddCommand(githubIntakeCmd)
	githubIntakeCmd.AddCommand(githubIntakeListCmd)
	githubIntakeListCmd.Flags().String("since", "", "RFC3339 time; only deliveries received at or after it (required)")
	githubIntakeListCmd.Flags().Int("limit", 50, "Maximum rows (1-500)")
	githubIntakeListCmd.Flags().String("output", "table", "Output format: table or json")
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

type githubIntakeDeliveries struct {
	Deliveries []struct {
		Event          string `json:"event"`
		DeliveryGUID   string `json:"delivery_guid"`
		State          string `json:"state"`
		Reason         string `json:"reason"`
		ReceivedAt     string `json:"received_at"`
		RevisionState  string `json:"revision_state"`
		RevisionReason string `json:"revision_reason"`
		Tasks          int    `json:"tasks"`
	} `json:"deliveries"`
}

func runGitHubIntakeList(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	since, _ := cmd.Flags().GetString("since")
	limit, _ := cmd.Flags().GetInt("limit")
	if since == "" {
		return fmt.Errorf("--since is required (RFC3339, e.g. 2026-10-30T10:31:00Z)")
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var resp githubIntakeDeliveries
	path := "/api/github/intake?" + url.Values{"since": {since}, "limit": {strconv.Itoa(limit)}}.Encode()
	if err := client.GetJSON(ctx, path, &resp); err != nil {
		return fmt.Errorf("read intake: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, resp)
	}
	if len(resp.Deliveries) == 0 {
		fmt.Println("no deliveries since", since)
		return nil
	}
	rows := make([][]string, 0, len(resp.Deliveries))
	for _, d := range resp.Deliveries {
		rows = append(rows, []string{d.Event, d.DeliveryGUID, d.State, d.Reason, d.RevisionState, d.RevisionReason, strconv.Itoa(d.Tasks), d.ReceivedAt})
	}
	cli.PrintTable(os.Stdout, []string{"EVENT", "DELIVERY", "STATE", "REASON", "REV_STATE", "REV_REASON", "RUNS", "RECEIVED_AT"}, rows)
	return nil
}
