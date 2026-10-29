package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// Webhook endpoint commands — manage the inbound endpoints a workflow's
// webhook(name) guards wait on (/api/issue-workflows/{id}/webhooks). The server
// gates every verb to the workflow's owner and workspace admins. The secret is
// printed once, by create and rotate, and cannot be read back.

var workflowWebhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Manage a workflow's inbound webhook endpoints (owner/admin only)",
}

var workflowWebhookCreateCmd = &cobra.Command{
	Use:   "create <workflow>",
	Short: "Create an endpoint; prints its secret once",
	Args:  exactArgs(1),
	RunE:  runWorkflowWebhookCreate,
}

var workflowWebhookRotateCmd = &cobra.Command{
	Use:   "rotate <workflow>",
	Short: "Issue a new secret; the old one stays valid for 24 hours",
	Args:  exactArgs(1),
	RunE:  runWorkflowWebhookRotate,
}

var workflowWebhookDisableCmd = &cobra.Command{
	Use:   "disable <workflow>",
	Short: "Stop an endpoint accepting events (cannot be undone; create a new one)",
	Args:  exactArgs(1),
	RunE:  runWorkflowWebhookDisable,
}

var workflowWebhookListCmd = &cobra.Command{
	Use:   "list <workflow>",
	Short: "List a workflow's endpoints (never shows secrets)",
	Args:  exactArgs(1),
	RunE:  runWorkflowWebhookList,
}

func init() {
	workflowCmd.AddCommand(workflowWebhookCmd)
	workflowWebhookCmd.AddCommand(workflowWebhookCreateCmd, workflowWebhookRotateCmd, workflowWebhookDisableCmd, workflowWebhookListCmd)

	for _, c := range []*cobra.Command{workflowWebhookCreateCmd, workflowWebhookRotateCmd, workflowWebhookDisableCmd} {
		c.Flags().String("name", "", "Endpoint name, as written in webhook(name) (required)")
		c.Flags().String("output", "json", "Output format: table or json")
	}
	workflowWebhookCreateCmd.Flags().Int("rate-per-min", 0, "Events accepted per minute, 1-6000 (0 keeps the default of 60)")
	workflowWebhookListCmd.Flags().String("output", "table", "Output format: table or json")
}

func workflowWebhooksPath(workflowID string) string {
	return "/api/issue-workflows/" + workflowID + "/webhooks"
}

func webhookName(cmd *cobra.Command) (string, error) {
	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		return "", errors.New("--name is required")
	}
	return name, nil
}

func runWorkflowWebhookCreate(cmd *cobra.Command, args []string) error {
	name, err := webhookName(cmd)
	if err != nil {
		return err
	}
	body := map[string]any{"name": name}
	if rate, _ := cmd.Flags().GetInt("rate-per-min"); rate != 0 {
		body["rate_per_min"] = rate
	}
	return changeWorkflowWebhook(cmd, args[0], "create", func(ctx context.Context, c *cli.APIClient) (map[string]any, error) {
		var ep map[string]any
		return ep, c.PostJSON(ctx, workflowWebhooksPath(args[0]), body, &ep)
	})
}

func runWorkflowWebhookRotate(cmd *cobra.Command, args []string) error {
	return actOnWorkflowWebhook(cmd, args[0], "rotate")
}

func runWorkflowWebhookDisable(cmd *cobra.Command, args []string) error {
	return actOnWorkflowWebhook(cmd, args[0], "disable")
}

// actOnWorkflowWebhook finds the endpoint by name, then posts the verb to it.
// An unknown name stops before any change is sent.
func actOnWorkflowWebhook(cmd *cobra.Command, workflowID, verb string) error {
	name, err := webhookName(cmd)
	if err != nil {
		return err
	}
	return changeWorkflowWebhook(cmd, workflowID, verb, func(ctx context.Context, c *cli.APIClient) (map[string]any, error) {
		endpoints, err := listWorkflowWebhooks(ctx, c, workflowID)
		if err != nil {
			return nil, err
		}
		for _, ep := range endpoints {
			if strVal(ep, "name") == name {
				var out map[string]any
				return out, c.PostJSON(ctx, workflowWebhooksPath(workflowID)+"/"+strVal(ep, "id")+"/"+verb, nil, &out)
			}
		}
		return nil, fmt.Errorf("workflow has no webhook endpoint named %q", name)
	})
}

func changeWorkflowWebhook(cmd *cobra.Command, _ string, verb string, do func(context.Context, *cli.APIClient) (map[string]any, error)) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	ep, err := do(ctx, client)
	if err != nil {
		var httpErr *cli.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%s webhook: only the workflow owner or admin can manage webhooks (server said: %s)", verb, httpErr.Body)
		}
		return fmt.Errorf("%s webhook: %w", verb, err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, ep)
	}
	fmt.Printf("%s  %s\n", strVal(ep, "id"), strVal(ep, "name"))
	if secret := strVal(ep, "secret"); secret != "" {
		fmt.Printf("Secret: %s\nThis is the only time the secret is shown. Store it now.\n", secret)
	}
	return nil
}

func listWorkflowWebhooks(ctx context.Context, client *cli.APIClient, workflowID string) ([]map[string]any, error) {
	var raw []map[string]any
	if err := client.GetJSON(ctx, workflowWebhooksPath(workflowID), &raw); err != nil {
		var httpErr *cli.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("only the workflow owner or admin can manage webhooks (server said: %s)", httpErr.Body)
		}
		return nil, err
	}
	return raw, nil
}

func runWorkflowWebhookList(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	endpoints, err := listWorkflowWebhooks(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("list webhooks: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, endpoints)
	}
	rows := make([][]string, 0, len(endpoints))
	for _, ep := range endpoints {
		state := "enabled"
		if strVal(ep, "disabled_at") != "" {
			state = "disabled"
		}
		rows = append(rows, []string{strVal(ep, "id"), strVal(ep, "name"), state, fmt.Sprint(ep["rate_per_min"]), strVal(ep, "previous_expires_at")})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "NAME", "STATE", "RATE/MIN", "PREVIOUS EXPIRES"}, rows)
	return nil
}
