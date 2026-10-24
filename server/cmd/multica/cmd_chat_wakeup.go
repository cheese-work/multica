package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var chatWakeupCmd = &cobra.Command{
	Use:   "wakeup",
	Short: "Schedule a follow-up turn in this web DM",
	Long: `Schedule a turn that starts in this DM later, so the person does not have to
ask again. The turn runs with the DM's full history and ends with a reply the
person sees as unread. Use it instead of telling the person to "refresh around
11:40".

A wakeup is due within 24 hours, and a DM holds at most 3 pending wakeups.
Only a web DM can be woken; a DM continued into a new session keeps its
wakeups. It reads only the conversation you are currently running for.`,
}

var chatWakeupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Schedule a wakeup (--at HH:MM|RFC3339 or --after DURATION)",
	Long: `Schedule a wakeup. --at takes an RFC3339 time, or HH:MM for the next
occurrence of that wall-clock time in --timezone (default: this host's zone).
--after takes a duration such as 20m. Write what to do in --instruction-file;
you will read it back when the wakeup fires.

With --until-issue the wakeup also waits for a fact, and --at/--after become the
deadline: the turn starts when the fact holds or at the deadline, whichever comes
first. Pick one fact: --until-status <status key>, --until-pr checks|merged (the
issue's linked pull request; checks needs a new finished result), or
--event comment.created --filter-actor-id <id> [--filter-actor-type member|agent]
for a new comment on the issue. --until-issue takes a UUID or an identifier.`,
	Args: cobra.NoArgs,
	RunE: runChatWakeupCreate,
}

var chatWakeupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List this DM's pending wakeups",
	Args:  cobra.NoArgs,
	RunE:  runChatWakeupList,
}

var chatWakeupCancelCmd = &cobra.Command{
	Use:   "cancel <id>",
	Short: "Cancel a pending wakeup",
	Args:  cobra.ExactArgs(1),
	RunE:  runChatWakeupCancel,
}

func init() {
	chatWakeupCreateCmd.Flags().String("at", "", "RFC3339 time, or HH:MM (next occurrence in --timezone)")
	chatWakeupCreateCmd.Flags().String("after", "", "Delay, e.g. 20m")
	chatWakeupCreateCmd.Flags().String("timezone", "", "IANA zone for HH:MM, e.g. Asia/Ho_Chi_Minh (default: this host's zone)")
	chatWakeupCreateCmd.Flags().String("until-issue", "", "Issue to watch (UUID or identifier); --at/--after become the deadline")
	chatWakeupCreateCmd.Flags().String("until-status", "", "Start when the watched issue has this status key")
	chatWakeupCreateCmd.Flags().String("until-pr", "", "Start on the watched issue's pull request: checks or merged")
	chatWakeupCreateCmd.Flags().String("event", "", "Start on an event on the watched issue: comment.created")
	chatWakeupCreateCmd.Flags().String("filter-actor-id", "", "With --event comment.created: the commenter's ID")
	chatWakeupCreateCmd.Flags().String("filter-actor-type", "member", "With --event comment.created: member or agent")
	chatWakeupCreateCmd.Flags().String("instruction", "", "What to do when the wakeup fires")
	chatWakeupCreateCmd.Flags().Bool("instruction-stdin", false, "Read the instruction from stdin")
	chatWakeupCreateCmd.Flags().String("instruction-file", "", "UTF-8 file with the instruction")
	for _, c := range []*cobra.Command{chatWakeupCreateCmd, chatWakeupListCmd, chatWakeupCancelCmd} {
		c.Flags().String("output", "json", "Output format: table or json")
		chatWakeupCmd.AddCommand(c)
	}
	chatCmd.AddCommand(chatWakeupCmd)
}

// parseChatWakeupAt reads an RFC3339 time, or HH:MM as the next occurrence of
// that wall-clock time in loc (tomorrow when today's has passed).
func parseChatWakeupAt(value string, now time.Time, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	clock, err := time.Parse("15:04", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("--at must be HH:MM or an RFC3339 time")
	}
	local := now.In(loc)
	t := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

// chatWakeupConditionFromFlags builds the API condition, or nil without
// --until-issue. It refuses a predicate without an issue and more or fewer than
// one predicate, before any request.
func chatWakeupConditionFromFlags(cmd *cobra.Command) (map[string]any, error) {
	issue, _ := cmd.Flags().GetString("until-issue")
	status, _ := cmd.Flags().GetString("until-status")
	pr, _ := cmd.Flags().GetString("until-pr")
	event, _ := cmd.Flags().GetString("event")
	actorID, _ := cmd.Flags().GetString("filter-actor-id")
	actorType, _ := cmd.Flags().GetString("filter-actor-type")
	predicates := map[string]any{}
	if status != "" {
		predicates["status"] = status
	}
	switch pr {
	case "":
	case "checks":
		predicates["pr"] = "checks_finished"
	case "merged":
		predicates["pr"] = "merged"
	default:
		return nil, fmt.Errorf("--until-pr must be checks or merged")
	}
	switch {
	case event == "" && actorID == "":
	case event != "comment.created":
		return nil, fmt.Errorf("--event must be comment.created, and it needs --filter-actor-id")
	case actorID == "":
		return nil, fmt.Errorf("--event comment.created needs --filter-actor-id")
	default:
		predicates["comment_by"] = map[string]any{"type": actorType, "id": actorID}
	}
	if issue == "" {
		if len(predicates) > 0 {
			return nil, fmt.Errorf("--until-status, --until-pr and --event need --until-issue")
		}
		return nil, nil
	}
	if len(predicates) != 1 {
		return nil, fmt.Errorf("--until-issue needs exactly one of --until-status, --until-pr or --event comment.created")
	}
	predicates["issue"] = issue
	return predicates, nil
}

func runChatWakeupCreate(cmd *cobra.Command, _ []string) error {
	at, _ := cmd.Flags().GetString("at")
	after, _ := cmd.Flags().GetString("after")
	zone, _ := cmd.Flags().GetString("timezone")
	if (at == "") == (after == "") {
		return fmt.Errorf("provide exactly one of --at or --after")
	}
	body := map[string]any{}
	if at != "" {
		loc := time.Local
		if zone != "" {
			var err error
			if loc, err = time.LoadLocation(zone); err != nil {
				return fmt.Errorf("invalid --timezone: %w", err)
			}
		}
		fireAt, err := parseChatWakeupAt(at, time.Now(), loc)
		if err != nil {
			return err
		}
		body["at"] = fireAt.Format(time.RFC3339)
	} else {
		d, err := time.ParseDuration(after)
		if err != nil || d <= 0 || d%time.Second != 0 {
			return fmt.Errorf("--after requires a positive whole-second duration")
		}
		body["after_seconds"] = int64(d / time.Second)
	}
	condition, err := chatWakeupConditionFromFlags(cmd)
	if err != nil {
		return err
	}
	if condition != nil {
		body["condition"] = condition
	}
	instruction, _, err := resolveTextFlag(cmd, "instruction")
	if err != nil {
		return err
	}
	body["instruction"] = instruction
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var row map[string]any
	if err := client.PostJSON(ctx, "/api/chat/wakeups", body, &row); err != nil {
		return fmt.Errorf("schedule wakeup: %w", err)
	}
	return printChatWakeups(cmd, row, []map[string]any{row})
}

func runChatWakeupList(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var resp struct {
		Wakeups []map[string]any `json:"wakeups"`
	}
	if err := client.GetJSON(ctx, "/api/chat/wakeups", &resp); err != nil {
		return fmt.Errorf("list wakeups: %w", err)
	}
	return printChatWakeups(cmd, resp, resp.Wakeups)
}

func runChatWakeupCancel(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var row map[string]any
	if err := client.DeleteJSONResponse(ctx, "/api/chat/wakeups/"+url.PathEscape(args[0]), &row); err != nil {
		return fmt.Errorf("cancel wakeup: %w", err)
	}
	return printChatWakeups(cmd, row, []map[string]any{row})
}

func printChatWakeups(cmd *cobra.Command, asJSON any, rows []map[string]any) error {
	if output, _ := cmd.Flags().GetString("output"); output != "table" {
		return cli.PrintJSON(os.Stdout, asJSON)
	}
	cells := make([][]string, 0, len(rows))
	for _, row := range rows {
		cells = append(cells, []string{strVal(row, "id"), strVal(row, "fire_at"), strVal(row, "status")})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "FIRE_AT", "STATUS"}, cells)
	return nil
}
