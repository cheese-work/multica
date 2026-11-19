package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// `multica browser` leases the shared Steel browser (CHE-4743, plan CHE-4695 F5). It never calls
// the server itself: the lease routes accept only the task's daemon credential, which the local
// daemon holds, so the call goes through the daemon with the agent's task token. The token a
// lease returns goes to stdout and nowhere else.

var browserCmd = &cobra.Command{
	Use:   "browser",
	Short: "Lease the shared remote browser (run inside a daemon task)",
}

var browserLeaseCmd = &cobra.Command{
	Use:   "lease --profile <profile-id> [--decision <decision-id>]",
	Short: "Ask for a browser lease; prints the gateway URL and token when granted",
	Long: `Ask for a lease on a browser profile. A granted lease prints the gateway URL and a
short-lived token on stdout; a queued lease prints its queue position and no token — run
"multica browser status <lease-id>" or "multica browser renew <lease-id>" to see the grant.
A login profile needs --decision, the id of an answered decision request on this issue.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		profile, _ := cmd.Flags().GetString("profile")
		decision, _ := cmd.Flags().GetString("decision")
		return runBrowserOp(cmd, browserOpRequest{Op: "lease", ProfileID: profile, DecisionID: decision})
	},
}

func browserLeaseIDCmd(op, short string) *cobra.Command {
	return &cobra.Command{
		Use:   op + " <lease-id>",
		Short: short,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrowserOp(cmd, browserOpRequest{Op: op, LeaseID: args[0]})
		},
	}
}

func init() {
	browserLeaseCmd.Flags().String("profile", "", "Browser profile id (required)")
	_ = browserLeaseCmd.MarkFlagRequired("profile")
	browserLeaseCmd.Flags().String("decision", "", "Answered decision request id (login profiles)")
	browserCmd.AddCommand(
		browserLeaseCmd,
		browserLeaseIDCmd("renew", "Renew a lease; prints a fresh token"),
		browserLeaseIDCmd("release", "End a lease you hold"),
		browserLeaseIDCmd("status", "Show a lease's state (never prints a token)"),
	)
	browserCmd.PersistentFlags().String("output", "table", "Output format: table or json")
}

type browserOpRequest struct {
	Op         string `json:"op"`
	ProfileID  string `json:"profile_id,omitempty"`
	DecisionID string `json:"decision_id,omitempty"`
	LeaseID    string `json:"lease_id,omitempty"`
}

func runBrowserOp(cmd *cobra.Command, op browserOpRequest) error {
	port := os.Getenv("MULTICA_DAEMON_PORT")
	if port == "" {
		return fmt.Errorf("MULTICA_DAEMON_PORT not set (this command is intended to be run by an agent inside a daemon task)")
	}
	token := os.Getenv("MULTICA_TOKEN")
	if token == "" {
		return fmt.Errorf("MULTICA_TOKEN not set (browser leases need the active task credential)")
	}
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+port+"/browser/lease", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("connect to daemon: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read daemon response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("browser %s failed (%d): %s", op.Op, resp.StatusCode, browserErrorText(body))
	}
	var lease map[string]any
	if err := json.Unmarshal(body, &lease); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if _, hasToken := lease["token"]; hasToken && lease["gateway_url"] == nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: the server has no gateway URL for this lease's host (MULTICA_BROWSER_GATEWAY_URLS)")
	}
	if out, _ := cmd.Flags().GetString("output"); out == "json" {
		return cli.PrintJSON(cmd.OutOrStdout(), lease)
	}
	w := cmd.OutOrStdout()
	for _, k := range []string{"lease_id", "state", "class", "queue_position", "revoked", "gateway_url", "token", "token_expires_at", "lease_expires_at"} {
		if v, ok := lease[k]; ok {
			fmt.Fprintf(w, "%s: %v\n", k, v)
		}
	}
	return nil
}

// browserErrorText keeps the server's code and message; any other body is cut short.
func browserErrorText(body []byte) string {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		if e.Code != "" {
			return e.Code + ": " + e.Error
		}
		return e.Error
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
