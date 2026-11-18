package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// CHE-4165 (CHE-4143 step 5): read and set the stack-autolink mode.

var githubStackAutolinkCmd = &cobra.Command{
	Use:   "stack-autolink",
	Short: "Show or set how stacked PRs are linked to each other (off, report or link)",
	Long: "Modes, lowest to highest: off < report < link. The default is report.\n" +
		"  off     never touch a PR.\n" +
		"  report  compute the stack and post a status only.\n" +
		"  link    also write the stack links into the PR.\n" +
		"A workspace set to off is a shutdown: a repo entry cannot raise it. Otherwise a repo entry wins over the workspace mode.\n" +
		"The final mode for a repo is the lowest effective mode across every workspace bound to the same GitHub App installation,\n" +
		"and the env var GITHUB_STACK_AUTOLINK=0 on the server forces off. `status` only reads. `set` needs a workspace owner or admin.",
}

var githubStackAutolinkStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the final mode for a repo and each workspace's effective mode (read-only)",
	Long: "Prints the final mode the server will act on for --repo, plus the effective mode of each workspace you belong to\n" +
		"that is bound to the repo owner's GitHub App installation. Workspaces you do not belong to count toward the final\n" +
		"mode but are only counted, never named.",
	Args: exactArgs(0),
	RunE: runGitHubStackAutolinkStatus,
}

var githubStackAutolinkSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set the stack-autolink mode for this workspace or one repo (owner or admin only)",
	Long: "Without --repo, sets the workspace mode. With --repo owner/repo, sets that repo's entry.\n" +
		"--mode is off, report or link. A workspace member who is not an owner or admin is refused.",
	Args: exactArgs(0),
	RunE: runGitHubStackAutolinkSet,
}

func init() {
	githubCmd.AddCommand(githubStackAutolinkCmd)
	githubStackAutolinkCmd.AddCommand(githubStackAutolinkStatusCmd, githubStackAutolinkSetCmd)
	githubStackAutolinkStatusCmd.Flags().String("repo", "", "Repository as owner/repo (required)")
	githubStackAutolinkStatusCmd.Flags().String("output", "table", "Output format: table or json")
	githubStackAutolinkSetCmd.Flags().String("repo", "", "Repository as owner/repo; omit to set the workspace mode")
	githubStackAutolinkSetCmd.Flags().String("mode", "", "off, report or link (required)")
	githubStackAutolinkSetCmd.Flags().String("output", "table", "Output format: table or json")
}

type githubStackAutolinkStatus struct {
	Repo             string `json:"repo"`
	FinalMode        string `json:"final_mode"`
	EnvKill          bool   `json:"env_kill"`
	HiddenWorkspaces int    `json:"hidden_workspaces"`
	Workspaces       []struct {
		WorkspaceID   string `json:"workspace_id"`
		Name          string `json:"name"`
		EffectiveMode string `json:"effective_mode"`
	} `json:"workspaces"`
}

type githubStackAutolinkSet struct {
	WorkspaceID   string `json:"workspace_id"`
	Repo          string `json:"repo,omitempty"`
	Mode          string `json:"mode"`
	EffectiveMode string `json:"effective_mode,omitempty"`
}

func runGitHubStackAutolinkStatus(cmd *cobra.Command, _ []string) error {
	repo, _ := cmd.Flags().GetString("repo")
	if repo == "" {
		return fmt.Errorf("--repo is required (owner/repo)")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var resp githubStackAutolinkStatus
	if err := client.GetJSON(ctx, "/api/github/stack-autolink?"+url.Values{"repo": {repo}}.Encode(), &resp); err != nil {
		return fmt.Errorf("read stack-autolink: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, resp)
	}
	fmt.Printf("%s: final mode %s\n", resp.Repo, resp.FinalMode)
	if resp.EnvKill {
		fmt.Println("GITHUB_STACK_AUTOLINK=0 is set on the server: everything is forced off")
	}
	if len(resp.Workspaces) > 0 {
		rows := make([][]string, 0, len(resp.Workspaces))
		for _, w := range resp.Workspaces {
			rows = append(rows, []string{w.Name, w.EffectiveMode})
		}
		cli.PrintTable(os.Stdout, []string{"WORKSPACE", "EFFECTIVE_MODE"}, rows)
	}
	switch resp.HiddenWorkspaces {
	case 0:
	case 1:
		fmt.Println("plus 1 other workspace you do not belong to; it counts toward the final mode")
	default:
		fmt.Printf("plus %d other workspaces you do not belong to; they count toward the final mode\n", resp.HiddenWorkspaces)
	}
	return nil
}

func runGitHubStackAutolinkSet(cmd *cobra.Command, _ []string) error {
	mode, _ := cmd.Flags().GetString("mode")
	switch mode {
	case "off", "report", "link":
	default:
		return fmt.Errorf("--mode must be off, report or link (got %q)", mode)
	}
	repo, _ := cmd.Flags().GetString("repo")
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	body := map[string]any{"mode": mode}
	if repo != "" {
		body["repo"] = repo
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var resp githubStackAutolinkSet
	if err := client.PutJSON(ctx, "/api/github/stack-autolink", body, &resp); err != nil {
		return fmt.Errorf("set stack-autolink: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, resp)
	}
	if resp.Repo != "" {
		fmt.Printf("%s: set to %s\n", resp.Repo, resp.Mode)
	} else {
		fmt.Printf("workspace: set to %s\n", resp.Mode)
	}
	return nil
}
