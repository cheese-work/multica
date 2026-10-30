package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

// The daily brief autopilot prints its "Agents decided — veto?" section from
// `multica decision brief`. The owner flips the pilot with `agent-setting`.
func init() {
	brief := &cobra.Command{Use: "brief", Short: "Read agent-made decisions for the daily brief: records, delivery failures and pilot metrics", Args: cobra.NoArgs, RunE: runAgentDecisionBrief}
	brief.Flags().String("format", "json", "json, or markdown for the brief section \"Agents decided — veto?\"")
	setting := &cobra.Command{Use: "agent-setting", Short: "Turn agent decisions on or off for the workspace (owner only; default off)", Args: cobra.NoArgs, RunE: runAgentDecisionSetting}
	setting.Flags().Bool("enabled", false, "true turns agent decisions on, false turns them off")
	_ = setting.MarkFlagRequired("enabled")
	decisionCmd.AddCommand(brief, setting)
}

func runAgentDecisionBrief(command *cobra.Command, _ []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if err := client.GetJSON(ctx, "/api/agent-decisions/brief", &out); err != nil {
		return err
	}
	format, _ := command.Flags().GetString("format")
	switch format {
	case "json":
		return cli.PrintJSON(os.Stdout, out)
	case "markdown":
		return writeAgentDecisionMarkdown(os.Stdout, out)
	}
	return fmt.Errorf("unknown --format %q: use json or markdown", format)
}

var ictZone = time.FixedZone("ICT", 7*60*60)

// The section stands on its own: the daily brief pastes it after Needs you,
// never inside it. Times are ICT.
func writeAgentDecisionMarkdown(w io.Writer, feed map[string]any) error {
	records, _ := feed["decisions"].([]any)
	failures, _ := feed["notice_failures"].([]any)
	stopped, isStopped := feed["stopped"].(map[string]any)
	// With the pilot off the brief stays as it was: no section. Records made
	// earlier stay vetoable, so they and a stop or failed notice still print.
	if enabled, _ := feed["enabled"].(bool); !enabled && len(records) == 0 && len(failures) == 0 && !isStopped {
		return nil
	}
	var b strings.Builder
	b.WriteString("## Agents decided — veto?\n\n")
	if isStopped {
		fmt.Fprintf(&b, "Agent decisions are stopped: %v (%s).\n\n", stopped["reason"], ictTime(stopped["at"]))
	}
	if len(records) == 0 {
		b.WriteString("No agent decisions to review.\n")
	}
	for _, raw := range records {
		rec, _ := raw.(map[string]any)
		fmt.Fprintf(&b, "- %v · %v · %v\n  Record: %v\n", rec["identifier"], rec["gate"], rec["state"], rec["link"])
		if rec["state"] == "revoked" {
			fmt.Fprintf(&b, "  Revoked: %v\n", rec["revoke_reason"])
			continue
		}
		fmt.Fprintf(&b, "  Review by %s (a target, not an expiry).\n  To veto, comment on the issue:\n  `%v` followed by your reason.\n", ictTime(rec["review_target_at"]), rec["veto_line"])
	}
	if len(failures) > 0 {
		b.WriteString("\nNotice delivery failed:\n")
		for _, raw := range failures {
			f, _ := raw.(map[string]any)
			fmt.Fprintf(&b, "- decision %v, %v attempts: %v\n", f["instance_id"], f["attempts"], f["last_error"])
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func ictTime(value any) string {
	text, _ := value.(string)
	t, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return text
	}
	return t.In(ictZone).Format("2006-01-02 15:04 ICT")
}

func runAgentDecisionSetting(command *cobra.Command, _ []string) error {
	enabled, _ := command.Flags().GetBool("enabled")
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if err := client.PutJSON(ctx, "/api/agent-decisions/setting", map[string]bool{"enabled": enabled}, &out); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, out)
}
