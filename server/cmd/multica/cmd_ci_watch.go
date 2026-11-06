package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var ciWatchCmd = &cobra.Command{Use: "ci-watch", Short: "Read and change the CI failure watcher settings of a repo (workspace admin)"}

// sliceFlags and intFlags map a CLI flag to its API field.
var (
	ciWatchStringFlags = map[string]string{"mode": "mode", "p1-mode": "p1_mode", "p2-mode": "p2_mode", "p3-mode": "p3_mode",
		"p4-mode": "p4_mode", "fix-owner-kind": "fix_owner_kind", "fix-owner-id": "fix_owner_id", "notify-member": "notify_member_id"}
	ciWatchListFlags = map[string]string{"branches": "branches", "events": "events",
		"workflows-include": "workflows_include", "workflows-exclude": "workflows_exclude"}
	ciWatchIntFlags = map[string]string{"max-reruns": "max_reruns_per_original_run",
		"max-new-issues": "max_new_issues_per_day", "max-owner-wakes": "max_owner_wakes_per_hour"}
)

func newCIWatchGetCmd() *cobra.Command {
	return &cobra.Command{Use: "get <owner/name>", Short: "Show the watcher settings of a repo", Args: cobra.ExactArgs(1), RunE: runCIWatchGet}
}

func newCIWatchSetCmd() *cobra.Command {
	c := &cobra.Command{Use: "set <owner/name>", Short: "Change watcher settings; only the flags you pass are sent", Args: cobra.ExactArgs(1), RunE: runCIWatchSet}
	f := c.Flags()
	f.String("mode", "", "Repo mode: off, shadow or act")
	for _, p := range []string{"p1", "p2", "p3", "p4"} {
		f.String(p+"-mode", "", "Protocol "+p+" mode: shadow or act")
	}
	f.StringSlice("branches", nil, "Branch globs to watch (comma-separated)")
	f.StringSlice("events", nil, "GitHub event names to watch (comma-separated)")
	f.StringSlice("workflows-include", nil, "Only these workflows (comma-separated)")
	f.StringSlice("workflows-exclude", nil, "Skip these workflows (comma-separated)")
	f.String("fix-owner-kind", "", "Fix owner kind: squad or agent (send with --fix-owner-id)")
	f.String("fix-owner-id", "", "Fix owner ID (send with --fix-owner-kind)")
	f.Bool("clear-fix-owner", false, "Remove the fix owner")
	f.String("notify-member", "", "Member (user ID) to notify")
	f.Bool("clear-notify-member", false, "Remove the notified member")
	f.Int("max-reruns", 0, "Reruns allowed per original run")
	f.Int("max-new-issues", 0, "New issues allowed per day")
	f.Int("max-owner-wakes", 0, "Owner wakes allowed per hour")
	return c
}

func newCIWatchEventsCmd() *cobra.Command {
	c := &cobra.Command{Use: "events <owner/name>", Short: "List recent watcher events of a repo", Args: cobra.ExactArgs(1), RunE: runCIWatchEvents}
	c.Flags().Int("limit", 0, "Number of events (1 to 200, server default 50)")
	return c
}

func init() {
	ciWatchCmd.AddCommand(newCIWatchGetCmd(), newCIWatchSetCmd(), newCIWatchEventsCmd())
	rootCmd.AddCommand(ciWatchCmd)
}

func ciWatchPath(command *cobra.Command, repo string) (string, error) {
	workspaceID, err := requireWorkspaceID(command)
	if err != nil {
		return "", err
	}
	return "/api/workspaces/" + url.PathEscape(workspaceID) + "/ci-watch/repos/" + url.PathEscape(repo), nil
}

func runCIWatchGet(command *cobra.Command, args []string) error {
	return ciWatchRequest(command, args[0], "", "get CI watch settings", func(ctx context.Context, c *cli.APIClient, path string, out *map[string]any) error {
		return c.GetJSON(ctx, path, out)
	})
}

func runCIWatchEvents(command *cobra.Command, args []string) error {
	suffix := "/events"
	if command.Flags().Changed("limit") {
		limit, _ := command.Flags().GetInt("limit")
		suffix += "?limit=" + strconv.Itoa(limit)
	}
	return ciWatchRequest(command, args[0], suffix, "list CI watch events", func(ctx context.Context, c *cli.APIClient, path string, out *map[string]any) error {
		return c.GetJSON(ctx, path, out)
	})
}

func runCIWatchSet(command *cobra.Command, args []string) error {
	body, err := ciWatchPatchBody(command)
	if err != nil {
		return err
	}
	return ciWatchRequest(command, args[0], "", "set CI watch settings", func(ctx context.Context, c *cli.APIClient, path string, out *map[string]any) error {
		return c.PutJSON(ctx, path, body, out)
	})
}

// ciWatchPatchBody turns the flags that were passed into the request body.
func ciWatchPatchBody(command *cobra.Command) (map[string]any, error) {
	flags := command.Flags()
	for _, pair := range [][2]string{{"notify-member", "clear-notify-member"}, {"fix-owner-kind", "clear-fix-owner"}, {"fix-owner-id", "clear-fix-owner"}} {
		if flags.Changed(pair[0]) && flags.Changed(pair[1]) {
			return nil, fmt.Errorf("--%s and --%s cannot be used together", pair[0], pair[1])
		}
	}
	body := map[string]any{}
	for flag, field := range ciWatchStringFlags {
		if flags.Changed(flag) {
			body[field], _ = flags.GetString(flag)
		}
	}
	for flag, field := range ciWatchListFlags {
		if flags.Changed(flag) {
			body[field], _ = flags.GetStringSlice(flag)
		}
	}
	for flag, field := range ciWatchIntFlags {
		if flags.Changed(flag) {
			body[field], _ = flags.GetInt(flag)
		}
	}
	if clear, _ := flags.GetBool("clear-notify-member"); clear {
		body["notify_member_id"] = nil
	}
	if clear, _ := flags.GetBool("clear-fix-owner"); clear {
		body["fix_owner_kind"], body["fix_owner_id"] = nil, nil
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("nothing to change: pass at least one setting flag")
	}
	return body, nil
}

func ciWatchRequest(command *cobra.Command, repo, suffix, what string, do func(context.Context, *cli.APIClient, string, *map[string]any) error) error {
	path, err := ciWatchPath(command, repo)
	if err != nil {
		return err
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	if err := do(ctx, client, path+suffix, &result); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return cli.PrintJSON(os.Stdout, result)
}
