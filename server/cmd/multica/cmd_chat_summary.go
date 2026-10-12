package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

var chatSummaryCmd = &cobra.Command{
	Use: "summary", Short: "Maintain the current DM lane description",
}

var chatSummarySetCmd = &cobra.Command{
	Use: "set", Short: "Set the current chat's lane summary from a UTF-8 file",
	Args: cobra.NoArgs, RunE: runChatSummarySet,
}

func init() {
	chatSummarySetCmd.Flags().String("file", "", "UTF-8 summary file (at most 600 Unicode characters; empty clears it)")
	_ = chatSummarySetCmd.MarkFlagRequired("file")
	chatSummaryCmd.AddCommand(chatSummarySetCmd)
	chatCmd.AddCommand(chatSummaryCmd)
}

func runChatSummarySet(cmd *cobra.Command, _ []string) error {
	path, _ := cmd.Flags().GetString("file")
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read summary file: %w", err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, 600*utf8.UTFMax+1))
	if err != nil {
		return fmt.Errorf("read summary file: %w", err)
	}
	if len(content) > 600*utf8.UTFMax {
		return fmt.Errorf("summary must not exceed 600 Unicode characters")
	}
	if !utf8.Valid(content) {
		return fmt.Errorf("summary file must contain valid UTF-8")
	}
	if utf8.RuneCount(content) > 600 {
		return fmt.Errorf("summary must not exceed 600 Unicode characters")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var response map[string]any
	if err := client.PutJSON(ctx, "/api/chat/summary", map[string]string{"lane_summary": string(content)}, &response); err != nil {
		return fmt.Errorf("set lane summary: %w", err)
	}
	return cli.PrintJSON(cmd.OutOrStdout(), response)
}
