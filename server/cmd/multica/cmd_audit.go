package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// `multica audit list` (CHE-5652) reads GET /api/workspaces/{id}/audit-events.
// Owner/admin only; the server enforces it.

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Read the workspace audit trail",
}

var auditListCmd = &cobra.Command{
	Use:   "list",
	Short: "List audit events, newest first (owner/admin only)",
	Args:  cobra.NoArgs,
	RunE:  runAuditList,
}

func init() {
	rootCmd.AddCommand(auditCmd)
	auditCmd.AddCommand(auditListCmd)

	f := auditListCmd.Flags()
	f.String("kind", "", "Kind prefix, e.g. agent. matches agent.update")
	f.String("target", "", "Target agent, by id or name")
	f.String("actor", "", "Actor id")
	f.String("since", "", "Only events after this: a duration (2h, 1d) or an RFC 3339 time")
	f.String("until", "", "Only events before this: a duration (2h, 1d) or an RFC 3339 time")
	f.Int("limit", 50, "Page size (server max 200)")
	f.Bool("all", false, "Follow next_cursor until the end")
	f.String("output", "table", "Output format: table or json")
}

// auditTime turns a --since/--until value into an RFC 3339 time: a Go duration
// (plus "d" for days) counts back from now; an RFC 3339 time is kept as is.
func auditTime(flag, v string, now time.Time) (string, error) {
	if v == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	d, err := parseDurationDays(v)
	if err != nil || d < 0 {
		return "", fmt.Errorf("invalid --%s %q: want a duration like 2h or 1d, or an RFC 3339 time", flag, v)
	}
	return now.Add(-d).UTC().Format(time.RFC3339Nano), nil
}

func parseDurationDays(v string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(v)
}

// auditQuery builds the query string from the flags; every check that can fail
// runs here, before any request is sent.
func auditQuery(ctx context.Context, cmd *cobra.Command, client *cli.APIClient) (url.Values, error) {
	f := cmd.Flags()
	q := url.Values{}
	if v, _ := f.GetString("kind"); v != "" {
		q.Set("kind", v)
	}
	if v, _ := f.GetString("actor"); v != "" {
		q.Set("actor_id", v)
	}
	if v, _ := f.GetString("target"); v != "" {
		id, err := resolveAgent(ctx, client, v)
		if err != nil {
			return nil, err
		}
		q.Set("target_id", id)
	}
	now := time.Now()
	for _, name := range []string{"since", "until"} {
		raw, _ := f.GetString(name)
		v, err := auditTime(name, raw, now)
		if err != nil {
			return nil, err
		}
		if v != "" {
			q.Set(name, v)
		}
	}
	if n, _ := f.GetInt("limit"); f.Changed("limit") {
		if n < 1 {
			return nil, fmt.Errorf("invalid --limit %d: must be at least 1", n)
		}
		q.Set("limit", strconv.Itoa(n))
	}
	return q, nil
}

type auditPage struct {
	Events     []json.RawMessage `json:"events"`
	NextCursor *string           `json:"next_cursor"`
}

func runAuditList(cmd *cobra.Command, _ []string) error {
	wsID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	q, err := auditQuery(ctx, cmd, client)
	if err != nil {
		return err
	}
	all, _ := cmd.Flags().GetBool("all")

	path := "/api/workspaces/" + wsID + "/audit-events"
	result := auditPage{Events: []json.RawMessage{}}
	for {
		var page auditPage
		if err := client.GetJSON(ctx, path+"?"+q.Encode(), &page); err != nil {
			return err
		}
		result.Events = append(result.Events, page.Events...)
		result.NextCursor = page.NextCursor
		if !all || page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		q.Set("cursor", *page.NextCursor)
	}
	if all {
		result.NextCursor = nil
	}

	if out, _ := cmd.Flags().GetString("output"); out == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	return printAuditTable(result)
}

func printAuditTable(p auditPage) error {
	rows := make([][]string, 0, len(p.Events))
	for _, raw := range p.Events {
		var e map[string]any
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("decode audit event: %w", err)
		}
		actor := orDash(strVal(e, "actor_name"))
		if t := strVal(e, "actor_type"); t != "" {
			actor += " (" + t + ")"
		}
		target := strVal(e, "target_name")
		if target == "" {
			target = truncateID(strVal(e, "target_id"))
		}
		rows = append(rows, []string{strVal(e, "at"), actor, strVal(e, "kind"), orDash(target)})
	}
	cli.PrintTable(os.Stdout, []string{"AT", "ACTOR", "KIND", "TARGET"}, rows)
	if p.NextCursor != nil && *p.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "more rows: pass --all, or --output json to read next_cursor %s\n", *p.NextCursor)
	}
	return nil
}
