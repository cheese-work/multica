package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

var issueDesignVerdictCmd = &cobra.Command{
	Use:   "design-verdict <issue> --attestation <id> --verdict PASS|CHANGES [--findings-file <path>]",
	Short: "Record this run's design review verdict on a doop attestation",
	Long: `Record a typed design review verdict for the issue's design, from the agent's own run.

The server reads the doop attestation, checks the issue's Design url against it
and checks that you are independent of the design author, then writes one
verdict record. A PASS that fails a check is refused with a code (HTTP 409).
A CHANGES verdict needs a findings file with at least one P0 or P1 finding.

--findings-file is a UTF-8 JSON array of {"severity":"P0|P1|P2|P3","summary":"..."}.
Only the agent's task token can record a verdict; a user token gets
design_verdict_run_required.`,
	Args: exactArgs(1),
	RunE: runIssueDesignVerdict,
}

func init() {
	issueDesignVerdictCmd.Flags().String("attestation", "", "doop design attestation id (required)")
	issueDesignVerdictCmd.Flags().String("verdict", "", "PASS or CHANGES (required)")
	issueDesignVerdictCmd.Flags().String("findings-file", "", "JSON array of {severity, summary}; CHANGES needs a P0 or P1 finding")
	issueDesignVerdictCmd.Flags().String("output", "json", "Output format: table or json")
	issueCmd.AddCommand(issueDesignVerdictCmd)
}

func runIssueDesignVerdict(command *cobra.Command, args []string) error {
	attestation, _ := command.Flags().GetString("attestation")
	verdict, _ := command.Flags().GetString("verdict")
	findingsPath, _ := command.Flags().GetString("findings-file")
	if strings.TrimSpace(attestation) == "" {
		return fmt.Errorf("--attestation is required")
	}
	if strings.TrimSpace(verdict) == "" {
		return fmt.Errorf("--verdict is required")
	}
	if verdict != "PASS" && verdict != "CHANGES" {
		return fmt.Errorf("--verdict must be PASS or CHANGES")
	}
	findings := []map[string]string{}
	if findingsPath != "" {
		raw, err := os.ReadFile(findingsPath)
		if err != nil {
			return fmt.Errorf("read findings file: %w", err)
		}
		if err := json.Unmarshal(raw, &findings); err != nil {
			return fmt.Errorf("findings file must be a JSON array of {severity, summary}: %w", err)
		}
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issue, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return err
	}
	var result map[string]any
	body := map[string]any{"attestation_id": attestation, "verdict": verdict, "findings": findings}
	if err := client.PostJSON(ctx, "/api/issues/"+url.PathEscape(issue.ID)+"/design-verdicts", body, &result); err != nil {
		return fmt.Errorf("design verdict not recorded: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}
