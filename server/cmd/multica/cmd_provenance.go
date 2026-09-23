package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var provenanceCmd = &cobra.Command{
	Use:   "provenance",
	Short: "Audited, read-only provenance exports",
}

var provenanceExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export issue and thread records as of a cutoff",
	Long: `Export a bounded, redacted package of issue and comment-thread records as
they stood at --cutoff. Read-only: nothing in the workspace is changed.

Only human workspace owners and admins may export; agent task tokens are
rejected. Every successful export writes an audit row (ids, revisions and
digests only) before any record is returned.

Every scope flag is explicit on purpose: --workspace never falls back to
--workspace-id, MULTICA_WORKSPACE_ID or the saved config.`,
	Example: `  # Export one issue and one thread as of a fixed instant
  $ multica provenance export --workspace <uuid> --issue MUL-123 \
      --thread <comment-uuid> --cutoff 2026-09-01T00:00:00Z

  # Summary instead of the full package
  $ multica provenance export --workspace <uuid> --issue MUL-123 \
      --cutoff 2026-09-01T00:00:00Z --output table`,
	Args: cobra.NoArgs,
	RunE: runProvenanceExport,
}

func init() {
	provenanceCmd.AddCommand(provenanceExportCmd)
	addProvenanceExportFlags(provenanceExportCmd)
}

func addProvenanceExportFlags(cmd *cobra.Command) {
	cmd.Flags().StringArray("workspace", nil, "Workspace UUID to export from (required, exactly one)")
	cmd.Flags().StringArray("issue", nil, "Issue UUID or identifier to export (repeatable)")
	cmd.Flags().StringArray("thread", nil, "Comment UUID whose thread to export (repeatable)")
	cmd.Flags().String("cutoff", "", "Export records as of this instant (RFC3339, required)")
	cmd.Flags().String("output", "json", "Output format: json or table")
}

type provenanceExportRequest struct {
	WorkspaceID string   `json:"workspace_id"`
	Issues      []string `json:"issues"`
	Threads     []string `json:"threads"`
	Cutoff      string   `json:"cutoff"`
}

type provenanceExportResult struct {
	ExportID        string           `json:"export_id"`
	RequestDigest   string           `json:"request_digest"`
	ManifestDigest  string           `json:"manifest_digest"`
	Cutoff          string           `json:"cutoff"`
	GeneratedAt     string           `json:"generated_at"`
	RedactionPolicy string           `json:"redaction_policy"`
	Records         []map[string]any `json:"records"`
	Exclusions      []map[string]any `json:"exclusions"`
}

func parseProvenanceExportFlags(cmd *cobra.Command) (provenanceExportRequest, string, error) {
	var req provenanceExportRequest
	workspaces, _ := cmd.Flags().GetStringArray("workspace")
	switch {
	case len(workspaces) == 0:
		return req, "", errors.New("--workspace is required")
	case len(workspaces) > 1:
		return req, "", fmt.Errorf("--workspace must be given exactly once, got %d", len(workspaces))
	case strings.TrimSpace(workspaces[0]) == "":
		return req, "", errors.New("--workspace must not be empty")
	}
	req.WorkspaceID = strings.TrimSpace(workspaces[0])

	raw, _ := cmd.Flags().GetString("cutoff")
	if strings.TrimSpace(raw) == "" {
		return req, "", errors.New("--cutoff is required")
	}
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		return req, "", fmt.Errorf("invalid --cutoff %q: expected RFC3339, e.g. 2026-08-19T00:00:00Z", raw)
	}
	req.Cutoff = raw

	req.Issues, _ = cmd.Flags().GetStringArray("issue")
	req.Threads, _ = cmd.Flags().GetStringArray("thread")
	if len(req.Issues) == 0 && len(req.Threads) == 0 {
		return req, "", errors.New("at least one --issue or --thread is required")
	}
	if req.Issues == nil {
		req.Issues = []string{}
	}
	if req.Threads == nil {
		req.Threads = []string{}
	}

	output, _ := cmd.Flags().GetString("output")
	if output != "json" && output != "table" {
		return req, "", fmt.Errorf("invalid --output %q: expected json or table", output)
	}
	return req, output, nil
}

func runProvenanceExport(cmd *cobra.Command, _ []string) error {
	req, output, err := parseProvenanceExportFlags(cmd)
	if err != nil {
		return fmt.Errorf("provenance export: %w", err)
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return fmt.Errorf("provenance export: %w", err)
	}
	// The workspace header must name the explicit --workspace, never the
	// ambient one newAPIClient resolved from flags/env/config.
	client.WorkspaceID = req.WorkspaceID

	ctx, cancel := context.WithTimeout(context.Background(), cli.AtLeastAPITimeout(60*time.Second))
	defer cancel()

	var result provenanceExportResult
	if err := client.PostJSON(ctx, "/api/provenance/export", req, &result); err != nil {
		return fmt.Errorf("provenance export: %w", err)
	}

	if output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	cli.PrintTable(os.Stdout, []string{"FIELD", "VALUE"}, [][]string{
		{"export_id", result.ExportID},
		{"cutoff", result.Cutoff},
		{"generated_at", result.GeneratedAt},
		{"records", strconv.Itoa(len(result.Records))},
		{"exclusions", strconv.Itoa(len(result.Exclusions))},
		{"request_digest", result.RequestDigest},
		{"manifest_digest", result.ManifestDigest},
		{"redaction_policy", result.RedactionPolicy},
	})
	return nil
}
