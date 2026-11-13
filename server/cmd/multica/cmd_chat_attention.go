package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

// DM attention CLI (CHE-4151, plan r4): the read side is the session list's projection fields;
// `later` snoozes the keys the server currently shows for the DM's chain.

var chatAttentionCmd = &cobra.Command{
	Use: "attention", Short: "List DMs that wait for you, oldest first",
	Args: cobra.NoArgs, RunE: runChatAttention,
}

var chatLaterCmd = &cobra.Command{
	Use: "later <session> [duration|time]", Short: "Hide a DM's open items until later (default 2h, max 7d)",
	Long: "Duration is 90m, 2h, 1d12h or seconds; a time is RFC3339. The server rejects values outside 1s..168h.\n" +
		"Retry a failed call with the same --request-id so it does not extend the time.",
	Args: cobra.RangeArgs(1, 2), RunE: runChatLater,
}

func init() {
	chatAttentionCmd.Flags().String("output", "table", "Output format: table or json")
	chatAttentionCmd.Flags().Bool("stale", false, "Only DMs that are yellow (4h) or red (24h)")
	chatLaterCmd.Flags().String("request-id", "", "UUID that makes a retry idempotent (default: new)")
	chatCmd.AddCommand(chatAttentionCmd, chatLaterCmd)
}

type chatAttentionRow struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Status          string  `json:"status,omitempty"`
	StaleLevel      string  `json:"stale_level"`
	AttentionSource *string `json:"attention_source"`
	AttentionSince  *string `json:"attention_since"`
	NextChangeAt    *string `json:"next_change_at"`
}

func runChatAttention(command *cobra.Command, _ []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var sessions []chatAttentionRow
	if err := client.GetJSON(ctx, "/api/chat/sessions", &sessions); err != nil {
		return fmt.Errorf("list chat sessions: %w", err)
	}
	stale, _ := command.Flags().GetBool("stale")
	rows := make([]chatAttentionRow, 0, len(sessions))
	for _, s := range sessions {
		visible := s.Status != "archived" && s.StaleLevel != "none" && s.StaleLevel != "" && s.AttentionSince != nil
		if visible && (!stale || s.StaleLevel == "yellow" || s.StaleLevel == "red") {
			rows = append(rows, s)
		}
	}
	since := func(r chatAttentionRow) time.Time { t, _ := time.Parse(time.RFC3339Nano, *r.AttentionSince); return t }
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := since(rows[i]), since(rows[j]); !a.Equal(b) {
			return a.Before(b)
		}
		return rows[i].ID < rows[j].ID
	})
	if output, _ := command.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, rows)
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		source := ""
		if r.AttentionSource != nil {
			source = *r.AttentionSource
		}
		table = append(table, []string{r.ID, r.Title, r.StaleLevel, source, *r.AttentionSince})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "TITLE", "LEVEL", "SOURCE", "SINCE"}, table)
	return nil
}

var laterPart = regexp.MustCompile(`^(\d+)d`)

// parseLaterSeconds reads "90m", "2h", "1d12h" or bare seconds. Range checks stay on the server.
func parseLaterSeconds(s string) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	var days int64
	if m := laterPart.FindStringSubmatch(s); m != nil {
		var err error
		// 1e6 days is far past the server's 168h cap; the guard only stops int64 wrap-around.
		if days, err = strconv.ParseInt(m[1], 10, 64); err != nil || days > 1_000_000 {
			return 0, fmt.Errorf("invalid duration %q (use 90m, 2h, 1d12h or seconds)", s)
		}
		s = s[len(m[0]):]
	}
	var rest time.Duration
	if s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (use 90m, 2h, 1d12h or seconds)", s)
		}
		rest = d
	}
	return days*86400 + int64(rest/time.Second), nil
}

func runChatLater(command *cobra.Command, args []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	body := map[string]any{"request_id": uuid.NewString()}
	if id, _ := command.Flags().GetString("request-id"); id != "" {
		body["request_id"] = id
	}
	if len(args) == 2 {
		if _, err := time.Parse(time.RFC3339, args[1]); err == nil {
			body["until"] = args[1]
		} else if body["duration_seconds"], err = parseLaterSeconds(args[1]); err != nil {
			return err
		}
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	base := "/api/chat/sessions/" + url.PathEscape(args[0]) + "/attention"
	var view struct {
		Attention struct {
			Keys []string `json:"keys"`
		} `json:"attention"`
	}
	if err := client.GetJSON(ctx, base, &view); err != nil {
		return fmt.Errorf("read chat attention: %w", err)
	}
	body["subject_keys"] = append([]string{}, view.Attention.Keys...)
	if err := client.PostJSON(ctx, base+"/later", body, nil); err != nil {
		return fmt.Errorf("set chat later: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Chat %s is later (request %s).\n", args[0], body["request_id"])
	return nil
}
