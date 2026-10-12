package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// maintenanceTokenEnv lets scripts keep the window token out of argv.
const maintenanceTokenEnv = "MULTICA_DAEMON_MAINTENANCE_TOKEN"

var daemonMaintenanceCmd = &cobra.Command{
	Use:   "maintenance",
	Short: "Hold the daemon idle (no new task claims) while you replace its binary or environment",
	Long: "A maintenance window pauses new task claims and takes the daemon's update ownership, so automatic\n" +
		"reload/update cannot restart it underneath you. Acquiring succeeds only when no claim is in flight and no\n" +
		"task is running; otherwise it exits non-zero and changes nothing. While the window is held,\n" +
		"`daemon stop` and `daemon restart` are refused unless given the window's --maintenance-token.\n" +
		"The window lives in the daemon process only and expires after --ttl; if the daemon dies it is gone,\n" +
		"so always confirm the outcome with `daemon status` rather than assuming a restart succeeded.",
}

var daemonMaintenanceAcquireCmd = &cobra.Command{
	Use:   "acquire",
	Short: "Acquire the maintenance window; prints {\"token\",\"expires_at\"} as JSON",
	Args:  exactArgs(0),
	RunE:  runDaemonMaintenanceAcquire,
}

var daemonMaintenanceReleaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Release the maintenance window and resume claiming",
	Args:  exactArgs(0),
	RunE:  runDaemonMaintenanceRelease,
}

var daemonMaintenanceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print daemon liveness and maintenance state as JSON (the token is never shown)",
	Args:  exactArgs(0),
	RunE:  runDaemonMaintenanceStatus,
}

func init() {
	daemonCmd.AddCommand(daemonMaintenanceCmd)
	daemonMaintenanceCmd.AddCommand(daemonMaintenanceAcquireCmd, daemonMaintenanceReleaseCmd, daemonMaintenanceStatusCmd)

	daemonMaintenanceAcquireCmd.Flags().Duration("ttl", 15*time.Minute, "Lease length; the window releases itself after this (max 1h)")
	tokenHelp := "Maintenance window token from `daemon maintenance acquire` (env: " + maintenanceTokenEnv + ")"
	daemonMaintenanceReleaseCmd.Flags().String("maintenance-token", "", tokenHelp)
	daemonStopCmd.Flags().String("maintenance-token", "", tokenHelp)
	daemonRestartCmd.Flags().String("maintenance-token", "", tokenHelp)
}

func maintenanceTokenFromCmd(cmd *cobra.Command) string {
	if v := strings.TrimSpace(flagString(cmd, "maintenance-token")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(maintenanceTokenEnv))
}

// daemonHealthPortForMaintenance resolves and identity-checks the daemon the
// command will act on, so a hashed-port collision cannot target another
// profile's daemon.
func daemonHealthPortForMaintenance(cmd *cobra.Command, command string) (int, map[string]any, error) {
	if err := requireHumanLocalCommand(command); err != nil {
		return 0, nil, err
	}
	profile := resolveProfile(cmd)
	if err := requireKnownProfile(profile); err != nil {
		return 0, nil, err
	}
	port := healthPortForProfile(profile)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	health := checkDaemonHealthOnPort(ctx, port)
	if !daemonAlive(health) {
		return 0, health, fmt.Errorf("daemon is not running")
	}
	if err := daemonIdentityMismatch(health, profile, port); err != nil {
		return 0, health, err
	}
	return port, health, nil
}

// postDaemonMaintenance POSTs to a /maintenance/* endpoint and returns the
// decoded body. A non-2xx answer is an error carrying the daemon's reason.
func postDaemonMaintenance(port int, path string, body any) (map[string]string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: maintenanceAcquireClientTimeout}
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d%s", port, path), "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := out["error"]
		if reason == "" {
			reason = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("daemon refused %s: %s", path, reason)
	}
	return out, nil
}

// Slightly longer than the daemon's own 30s admission wait, so the daemon's
// "busy" answer wins over a client timeout.
const maintenanceAcquireClientTimeout = 40 * time.Second

func runDaemonMaintenanceAcquire(cmd *cobra.Command, _ []string) error {
	port, _, err := daemonHealthPortForMaintenance(cmd, "daemon maintenance acquire")
	if err != nil {
		return err
	}
	ttl, _ := cmd.Flags().GetDuration("ttl")
	if ttl <= 0 {
		return fmt.Errorf("--ttl must be positive")
	}
	out, err := postDaemonMaintenance(port, "/maintenance/acquire", map[string]int{"ttl_seconds": int(ttl.Seconds())})
	if err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, out)
}

func runDaemonMaintenanceRelease(cmd *cobra.Command, _ []string) error {
	port, _, err := daemonHealthPortForMaintenance(cmd, "daemon maintenance release")
	if err != nil {
		return err
	}
	token := maintenanceTokenFromCmd(cmd)
	if token == "" {
		return fmt.Errorf("--maintenance-token (or %s) is required", maintenanceTokenEnv)
	}
	out, err := postDaemonMaintenance(port, "/maintenance/release", map[string]string{"token": token})
	if err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, out)
}

func runDaemonMaintenanceStatus(cmd *cobra.Command, _ []string) error {
	if err := requireHumanLocalCommand("daemon maintenance status"); err != nil {
		return err
	}
	profile := resolveProfile(cmd)
	if err := requireKnownProfile(profile); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	health := checkDaemonHealthOnPort(ctx, healthPortForProfile(profile))
	return cli.PrintJSON(os.Stdout, map[string]any{
		"status":                 health["status"],
		"pid":                    health["pid"],
		"active_task_count":      health["active_task_count"],
		"maintenance_held":       health["maintenance_held"] == true,
		"maintenance_expires_at": health["maintenance_expires_at"],
	})
}
