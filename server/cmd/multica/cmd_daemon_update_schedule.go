package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// Per-machine update schedule (CHE-5465). The schedule belongs to the machine
// the runtime runs on, so every runtime on that machine reads and writes the
// same one.

var daemonUpdateScheduleCmd = &cobra.Command{
	Use:   "update-schedule",
	Short: "Show, set or clear a machine's scheduled self-update",
	Long: "A machine can update its daemon on a cron schedule inside a maintenance window " +
		"of 10 to 30 minutes. The schedule belongs to the machine, so --runtime-id names " +
		"any runtime on it. Only the machine owner and the workspace owner can change it.",
}

var daemonUpdateScheduleGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Show the machine's update schedule and its next 3 fire times",
	Args:  exactArgs(0),
	RunE:  runDaemonUpdateScheduleGet,
}

var daemonUpdateScheduleSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Save the machine's update schedule",
	Example: "  multica daemon update-schedule set --runtime-id <id> --cron '0 2 * * *'\n" +
		"  multica daemon update-schedule set --runtime-id <id> --cron '30 3 * * 1' --timezone UTC --window-minutes 15 --enabled=false",
	Args: exactArgs(0),
	RunE: runDaemonUpdateScheduleSet,
}

var daemonUpdateScheduleClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Remove the machine's update schedule",
	Args:  exactArgs(0),
	RunE:  runDaemonUpdateScheduleClear,
}

func addDaemonUpdateScheduleFlags(cmd *cobra.Command) {
	cmd.Flags().String("runtime-id", "", "A runtime on the machine (required)")
	cmd.Flags().String("output", "table", "Output format: table or json")
	if cmd.Name() != "get" && cmd.Name() != "clear" {
		cmd.Flags().String("cron", "", "Five-field cron, e.g. '0 2 * * *' (required)")
		cmd.Flags().String("timezone", "", "IANA time zone (default Asia/Ho_Chi_Minh)")
		cmd.Flags().Int("window-minutes", 0, "Maintenance window in minutes, 10 to 30 (default 30)")
		cmd.Flags().Bool("enabled", true, "Run the schedule; --enabled=false saves it paused")
	}
}

func init() {
	for _, c := range []*cobra.Command{daemonUpdateScheduleGetCmd, daemonUpdateScheduleSetCmd, daemonUpdateScheduleClearCmd} {
		addDaemonUpdateScheduleFlags(c)
		daemonUpdateScheduleCmd.AddCommand(c)
	}
	daemonCmd.AddCommand(daemonUpdateScheduleCmd)
}

func updateSchedulePath(cmd *cobra.Command) (string, error) {
	id, _ := cmd.Flags().GetString("runtime-id")
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("--runtime-id is required")
	}
	return "/api/runtimes/" + url.PathEscape(strings.TrimSpace(id)) + "/update-schedule", nil
}

func printUpdateSchedule(cmd *cobra.Command, sched map[string]any) error {
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, sched)
	}
	cron := strVal(sched, "cron")
	if cron == "" {
		fmt.Println("No update schedule is saved for this machine.")
		return nil
	}
	enabled := "paused"
	if b, _ := sched["enabled"].(bool); b {
		enabled = "enabled"
	}
	fmt.Printf("Schedule:  %s (%s)\n", cron, enabled)
	fmt.Printf("Time zone: %s\n", strVal(sched, "timezone"))
	if w, ok := sched["window_minutes"].(float64); ok {
		fmt.Printf("Window:    %d min\n", int(w))
	}
	if runs, _ := sched["next_runs"].([]any); len(runs) > 0 {
		fmt.Println("Next runs:")
		for _, r := range runs {
			fmt.Printf("  %v\n", r)
		}
	}
	return nil
}

func runDaemonUpdateScheduleGet(cmd *cobra.Command, _ []string) error {
	path, err := updateSchedulePath(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var sched map[string]any
	if err := client.GetJSON(ctx, path, &sched); err != nil {
		return fmt.Errorf("get update schedule: %w", err)
	}
	return printUpdateSchedule(cmd, sched)
}

func runDaemonUpdateScheduleSet(cmd *cobra.Command, _ []string) error {
	path, err := updateSchedulePath(cmd)
	if err != nil {
		return err
	}
	cronExpr, _ := cmd.Flags().GetString("cron")
	if strings.TrimSpace(cronExpr) == "" {
		return fmt.Errorf("--cron is required")
	}
	enabled, _ := cmd.Flags().GetBool("enabled")
	body := map[string]any{"enabled": enabled, "cron": cronExpr}
	// An unset flag is left out so the server applies its own default.
	if cmd.Flags().Changed("timezone") {
		tz, _ := cmd.Flags().GetString("timezone")
		body["timezone"] = tz
	}
	if cmd.Flags().Changed("window-minutes") {
		w, _ := cmd.Flags().GetInt("window-minutes")
		body["window_minutes"] = w
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var sched map[string]any
	if err := client.PutJSON(ctx, path, body, &sched); err != nil {
		return fmt.Errorf("set update schedule: %w", err)
	}
	return printUpdateSchedule(cmd, sched)
}

func runDaemonUpdateScheduleClear(cmd *cobra.Command, _ []string) error {
	path, err := updateSchedulePath(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteJSON(ctx, path); err != nil {
		return fmt.Errorf("clear update schedule: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, map[string]any{"cleared": true})
	}
	fmt.Println("Update schedule cleared.")
	return nil
}
