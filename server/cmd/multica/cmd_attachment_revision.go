package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var attachmentHistoryCmd = &cobra.Command{
	Use:   "history <attachment-id>",
	Short: "List every revision of an attachment (time, actor, SHA-256)",
	Args:  exactArgs(1),
	RunE:  runAttachmentHistory,
}

var attachmentDiffCmd = &cobra.Command{
	Use:   "diff <attachment-id> --from N --to M",
	Short: "Print a unified diff between two revisions of an attachment",
	Long: `Print a unified diff between two revisions of a text attachment
(md, html, json, yaml, code). A binary file prints "binary, sizes and SHA differ".`,
	Args: exactArgs(1),
	RunE: runAttachmentDiff,
}

// runAttachmentUploadToIssue uploads a file to an issue. With replaces it is
// the next revision of that attachment; the issue then defaults to the
// replaced file's issue.
func runAttachmentUploadToIssue(cmd *cobra.Command, client *cli.APIClient, path, issueRef, replaces string) error {
	if isHTTPURL(path) {
		return fmt.Errorf("upload accepts a local file path, not a URL: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file %s: %w", path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cli.AtLeastAPITimeout(60*time.Second))
	defer cancel()

	issueID := ""
	switch {
	case issueRef != "":
		ref, err := resolveIssueRef(ctx, client, issueRef)
		if err != nil {
			return fmt.Errorf("resolve issue: %w", err)
		}
		issueID = ref.ID
	default:
		var prev map[string]any
		if err := client.GetJSON(ctx, "/api/attachments/"+replaces, &prev); err != nil {
			return fmt.Errorf("get replaced attachment: %w", err)
		}
		if issueID = strVal(prev, "issue_id"); issueID == "" {
			return fmt.Errorf("attachment %s is not on an issue; pass --issue", replaces)
		}
	}

	att, err := client.UploadIssueAttachmentReplacing(ctx, data, path, issueID, replaces)
	if err != nil {
		return fmt.Errorf("upload attachment: %w", err)
	}
	filename := filepath.Base(path)
	fmt.Fprintf(os.Stderr, "Uploaded: %s (v%d)\n", filename, att.Revision)
	result := map[string]any{
		"id":           att.ID,
		"filename":     filename,
		"revision":     att.Revision,
		"markdown_url": att.MarkdownURL,
		"markdown":     attachmentMarkdown(filename, att.ContentType, att.MarkdownURL),
	}
	if att.WebURL != "" {
		result["web_url"] = att.WebURL
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runAttachmentHistory(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var result map[string]any
	if err := client.GetJSON(ctx, "/api/attachments/"+args[0]+"/history", &result); err != nil {
		return fmt.Errorf("attachment history: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	revisions, _ := result["revisions"].([]any)
	rows := make([][]string, 0, len(revisions))
	for _, item := range revisions {
		rev, _ := item.(map[string]any)
		n, _ := rev["revision"].(float64)
		actor := strVal(rev, "uploader_type") + ":" + strVal(rev, "uploader_id")
		rows = append(rows, []string{strconv.FormatInt(int64(n), 10), ictTime(rev["created_at"]), actor, strVal(rev, "sha256")})
	}
	cli.PrintTable(os.Stdout, []string{"REVISION", "TIME", "ACTOR", "SHA-256"}, rows)
	return nil
}

func runAttachmentDiff(cmd *cobra.Command, args []string) error {
	from, _ := cmd.Flags().GetInt64("from")
	to, _ := cmd.Flags().GetInt64("to")
	if from < 1 {
		return fmt.Errorf("--from is required (revision number, 1 or more)")
	}
	if to < 1 {
		return fmt.Errorf("--to is required (revision number, 1 or more)")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	q := url.Values{"from": {strconv.FormatInt(from, 10)}, "to": {strconv.FormatInt(to, 10)}}
	var result map[string]any
	if err := client.GetJSON(ctx, "/api/attachments/"+args[0]+"/diff?"+q.Encode(), &result); err != nil {
		return fmt.Errorf("attachment diff: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	if binary, _ := result["binary"].(bool); binary {
		fmt.Fprintln(os.Stdout, strVal(result, "message"))
		return nil
	}
	fmt.Fprint(os.Stdout, strVal(result, "diff"))
	return nil
}
