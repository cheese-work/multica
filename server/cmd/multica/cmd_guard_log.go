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

// The Guard settings page and these commands read the same server rows
// (CHE-5207), so they show the same numbers.

var guardLogCmd = &cobra.Command{
	Use:   "log",
	Short: "List recent guard decisions, newest first",
	Args:  cobra.NoArgs,
	RunE:  runGuardLog,
}

var guardStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Count guard decisions per plugin (fixed, refused, errors) for the last days",
	Args:  cobra.NoArgs,
	RunE:  runGuardStats,
}

func init() {
	guardLogCmd.Flags().String("plugin", "", "Only this plugin")
	guardLogCmd.Flags().String("action", "", "Only this outcome: fixed, refused, plugin-error, shadow-hit or pass")
	guardLogCmd.Flags().String("since", "7d", "Window: a day count, a duration (24h, 7d) or an RFC 3339 time")
	guardLogCmd.Flags().Int("limit", 50, "Most decisions to return (1 to 500)")
	guardStatsCmd.Flags().Int("days", 7, "Window in days (1 to 90)")
	guardCmd.AddCommand(guardLogCmd, guardStatsCmd)
}

func guardGet(cmd *cobra.Command, path string, q url.Values, out any) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return client.GetJSON(ctx, path, out)
}

func wantsTable(cmd *cobra.Command) bool {
	out, _ := cmd.Flags().GetString("output")
	return out == "table"
}

func runGuardLog(cmd *cobra.Command, _ []string) error {
	q := url.Values{}
	for _, f := range []string{"plugin", "action", "since"} {
		if v, _ := cmd.Flags().GetString(f); v != "" {
			q.Set(f, v)
		}
	}
	if n, _ := cmd.Flags().GetInt("limit"); n > 0 {
		q.Set("limit", strconv.Itoa(n))
	}
	var resp struct {
		Decisions []map[string]any `json:"decisions"`
	}
	if err := guardGet(cmd, "/api/guard/decisions", q, &resp); err != nil {
		return err
	}
	if resp.Decisions == nil {
		resp.Decisions = []map[string]any{}
	}
	if !wantsTable(cmd) {
		return cli.PrintJSON(os.Stdout, resp.Decisions)
	}
	table := make([][]string, 0, len(resp.Decisions))
	for _, d := range resp.Decisions {
		table = append(table, []string{str(d["time"]), str(d["plugin"]), str(d["action"]), str(d["runtime"]), str(d["issue"]), str(d["line"])})
	}
	cli.PrintTable(os.Stdout, []string{"TIME (UTC)", "PLUGIN", "OUTCOME", "RUNTIME", "ISSUE", "LINE"}, table)
	return nil
}

func runGuardStats(cmd *cobra.Command, _ []string) error {
	q := url.Values{}
	if n, _ := cmd.Flags().GetInt("days"); n > 0 {
		q.Set("days", strconv.Itoa(n))
	}
	var resp struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := guardGet(cmd, "/api/guard/stats", q, &resp); err != nil {
		return err
	}
	if resp.Plugins == nil {
		resp.Plugins = []map[string]any{}
	}
	if !wantsTable(cmd) {
		return cli.PrintJSON(os.Stdout, resp.Plugins)
	}
	table := make([][]string, 0, len(resp.Plugins))
	for _, p := range resp.Plugins {
		table = append(table, []string{str(p["plugin"]), str(p["fixed"]), str(p["refused"]), str(p["errors"])})
	}
	cli.PrintTable(os.Stdout, []string{"PLUGIN", "FIXED", "REFUSED", "ERRORS"}, table)
	return nil
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
