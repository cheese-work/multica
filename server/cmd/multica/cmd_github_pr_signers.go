package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// CHE-10268 (S3 of CHE-10198): read the verified signers of one PR head.

var githubPRSignersCmd = &cobra.Command{
	Use:   "pr-signers",
	Short: "Show the verified PASS signers on the exact head of a pull request",
	Long: "Prints the distinct agents whose trusted PASS verdict stands on --head, in a comment on --issue, plus the PR author and whether comment protection is on.\n" +
		"A PASS is trusted when an agent run wrote it, nobody edited or deleted it, it is on the exact head and no newer verdict from the same agent withdrew it. The PR author never counts.\n" +
		"The server returns facts only; any merge rule lives in the caller. Exit is non-zero when the server answers 400 (bad repo, pr, head or issue),\n" +
		"404 (PR not stored, or no such issue) or 409 (the issue is not linked to the PR).",
	Example: "  multica github pr-signers --repo OWNER/NAME --pr 206 --head <40-char-sha> --issue CHE-10198 --output json",
	Args:    exactArgs(0),
	RunE:    runGitHubPRSigners,
}

func init() {
	githubCmd.AddCommand(githubPRSignersCmd)
	githubPRSignersCmd.Flags().String("repo", "", "Repository as OWNER/NAME (required)")
	githubPRSignersCmd.Flags().Int("pr", 0, "Pull request number (required)")
	githubPRSignersCmd.Flags().String("head", "", "Exact 40-character head commit SHA (required)")
	githubPRSignersCmd.Flags().String("issue", "", "Issue identifier or UUID the PASS comments are on (required)")
	githubPRSignersCmd.Flags().String("output", "table", "Output format: table or json")
}

type githubPRSigners struct {
	Repo              string `json:"repo"`
	PR                int    `json:"pr"`
	HeadSHA           string `json:"head_sha"`
	CommentProtection bool   `json:"comment_protection"`
	PRAuthor          *struct {
		AgentName string `json:"agent_name"`
		Lab       string `json:"lab"`
	} `json:"pr_author"`
	Signers []struct {
		AgentName string `json:"agent_name"`
		Lab       string `json:"lab"`
		CommentID string `json:"comment_id"`
		VerdictAt string `json:"verdict_at"`
	} `json:"signers"`
}

func runGitHubPRSigners(cmd *cobra.Command, _ []string) error {
	repo, _ := cmd.Flags().GetString("repo")
	pr, _ := cmd.Flags().GetInt("pr")
	head, _ := cmd.Flags().GetString("head")
	issue, _ := cmd.Flags().GetString("issue")
	switch {
	case repo == "":
		return fmt.Errorf("--repo is required (OWNER/NAME)")
	case pr < 1:
		return fmt.Errorf("--pr is required (a positive number)")
	case head == "":
		return fmt.Errorf("--head is required (the exact 40-character SHA)")
	case issue == "":
		return fmt.Errorf("--issue is required")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var raw json.RawMessage
	query := url.Values{"repo": {repo}, "pr": {strconv.Itoa(pr)}, "head": {head}, "issue": {issue}}
	if err := client.GetJSON(ctx, "/api/github/pull-requests/signers?"+query.Encode(), &raw); err != nil {
		return fmt.Errorf("read pr signers: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, raw)
	}
	var resp githubPRSigners
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("read pr signers: %w", err)
	}
	fmt.Printf("%s#%d head %s\n", resp.Repo, resp.PR, resp.HeadSHA)
	if resp.PRAuthor != nil {
		fmt.Printf("author: %s (%s)\n", resp.PRAuthor.AgentName, labOrUnknown(resp.PRAuthor.Lab))
	} else {
		fmt.Println("author: unknown")
	}
	if resp.CommentProtection {
		fmt.Println("comment protection: on")
	} else {
		fmt.Println("comment protection: off")
	}
	rows := make([][]string, 0, len(resp.Signers))
	for _, s := range resp.Signers {
		rows = append(rows, []string{s.AgentName, labOrUnknown(s.Lab), s.CommentID, s.VerdictAt})
	}
	cli.PrintTable(os.Stdout, []string{"SIGNER", "LAB", "COMMENT", "VERDICT_AT"}, rows)
	return nil
}

func labOrUnknown(lab string) string {
	if lab == "" {
		return "unknown lab"
	}
	return lab
}
