package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

var githubRepoCreateCmd = &cobra.Command{
	Use:   "repo-create --org <org> --name <name> --out <action.json>",
	Short: "Write the decision action file that asks to create a GitHub repository",
	Long: `Validate a repository request and write the action file for
"multica decision request --action-file". Nothing is sent and no repository is
created until the workspace owner approves the card with a passkey. The server
then creates the repository with an org-admin credential this CLI never sees,
and adds it to the guard repository allowlist (when the org has one) and to the
workspace repo registry.

Only approved orgs are accepted (tevo-studio, cheese-work). A template must come
from an approved org. The repository is private unless --visibility says
otherwise; a public repository is a publication, so ask with
"multica decision request <issue> --action-file <out> --class publish".
Use --dry-run to print the action and write nothing.`,
	Args: exactArgs(0),
	RunE: runGitHubRepoCreate,
}

func init() {
	githubCmd.AddCommand(githubRepoCreateCmd)
	flags := githubRepoCreateCmd.Flags()
	flags.String("org", "", "GitHub org to create the repository in (required)")
	flags.String("name", "", "Repository name (required)")
	flags.String("template", "", "Template repository as owner/repo (optional)")
	flags.Bool("private", false, "Create a private repository (the default)")
	flags.String("visibility", "", "private, internal or public (default private)")
	flags.String("description", "", "One-line repository description")
	flags.String("out", "", "Where to write the action file")
	flags.Bool("dry-run", false, "Print the action; write nothing")
	flags.Int("expires-in", 0, "Seconds the request stays open (0 = server default)")
}

func runGitHubRepoCreate(cmd *cobra.Command, _ []string) error {
	org, _ := cmd.Flags().GetString("org")
	name, _ := cmd.Flags().GetString("name")
	template, _ := cmd.Flags().GetString("template")
	description, _ := cmd.Flags().GetString("description")
	visibility, _ := cmd.Flags().GetString("visibility")
	private, _ := cmd.Flags().GetBool("private")
	out, _ := cmd.Flags().GetString("out")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	expires, _ := cmd.Flags().GetInt("expires-in")
	if org == "" || name == "" {
		return errors.New("--org and --name are required")
	}
	if !dryRun && out == "" {
		return errors.New("--out is required (or use --dry-run)")
	}
	if private && visibility != "" && visibility != "private" {
		return fmt.Errorf("--private conflicts with --visibility %s", visibility)
	}
	if visibility == "" {
		visibility = "private"
	}
	workspaceID := resolveWorkspaceID(cmd)
	if workspaceID == "" {
		return errors.New("workspace not set: use --workspace-id or MULTICA_WORKSPACE_ID")
	}
	params := map[string]any{"workspace_id": workspaceID, "org": org, "name": name, "visibility": visibility}
	if template != "" {
		params["template"] = template
	}
	if description != "" {
		params["description"] = description
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	// Refuse what the server's allow table would refuse, before writing anything.
	if _, err := decisionaction.Normalize(decisionaction.Request{Type: decisionaction.GitHubRepoCreate, Version: 1, Params: raw}); err != nil {
		return err
	}
	action := map[string]any{"type": decisionaction.GitHubRepoCreate, "version": 1, "params": params}
	if expires > 0 {
		action["expires_in_seconds"] = expires
	}
	encoded, err := marshalIndent(action)
	if err != nil {
		return err
	}
	class := ""
	if visibility == "public" {
		class = " --class publish"
	}
	if dryRun {
		fmt.Printf("%s\n", encoded)
		if class != "" {
			fmt.Printf("note: a public repository needs: multica decision request <issue> --action-file <out>%s\n", class)
		}
		return nil
	}
	if err := os.WriteFile(out, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write action file: %w", err)
	}
	fmt.Printf("wrote %s\nnext: multica decision request <issue> --action-file %s%s\n", out, out, class)
	return nil
}
