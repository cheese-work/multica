package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Work with the current chat conversation",
}

var chatContinueCmd = &cobra.Command{
	Use:   "continue",
	Short: "Continue this web DM in a fresh session seeded with a summary",
	Long: `Without --fork, continue archives this web DM and moves the conversation to a
new DM seeded with the --summary-file summary.

With --fork, this DM stays active and unchanged. A new child DM receives a
copy of its visible messages and attachments, with no summary and no message
sent. --summary-file is not accepted with --fork. A retry of the same turn
returns the same child.`,
	Args: cobra.NoArgs,
	RunE: runChatContinue,
}

var chatCompactCmd = &cobra.Command{
	Use:   "compact",
	Short: "Compact this web DM in place with a summary",
	Long: `Compact stores the --summary-file summary as the opening of the DM's next
session. The DM keeps its id, history and URL; the agent's next turn starts from
the summary instead of the full transcript.

Unlike "continue", it never archives the DM or creates a new one. If the server
does not support in-place compaction the command stops and the DM is unchanged.`,
	Args: cobra.NoArgs,
	RunE: runChatCompact,
}

var chatHistoryCmd = &cobra.Command{
	Use:   "history",
	Short: "Overview of the channel this conversation is in (messages + thread list)",
	Long: `Show the overview of the chat channel (e.g. Slack) this conversation is in: the
recent top-level messages, and for each thread its thread_id, reply_count, and
latest_reply. It does NOT expand thread contents — it is the table of contents.

To read a specific thread's messages, take a thread_id from here and run
"multica chat thread <thread_id>".

It is the SAME command regardless of which channel the conversation came from,
and it reads only the conversation you are currently running for — it cannot
read any other session or channel.`,
	Args: cobra.NoArgs,
	RunE: runChatHistory,
}

var chatThreadCmd = &cobra.Command{
	Use:   "thread [id]",
	Short: "Read one thread's messages (the current thread, or a specific id)",
	Long: `Read the messages of a single thread.

With no id, read the thread you are currently in (the one you were @mentioned in).
With an id — a thread_id from "multica chat history" — read that specific thread.
Either way the thread is within the channel you are in; you cannot read another
channel.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runChatThread,
}

func init() {
	chatContinueCmd.Flags().String("summary-file", "", "UTF-8 handoff summary file (required without --fork)")
	chatContinueCmd.Flags().Bool("fork", false, "Copy this DM into a new child DM and keep this DM active")
	chatContinueCmd.Flags().String("idempotency-key", "", "Fork operation key (defaults to the current chat task ID)")
	chatContinueCmd.Flags().String("output", "json", "Output format: table or json")
	chatCmd.AddCommand(chatContinueCmd)
	chatCompactCmd.Flags().String("summary-file", "", "UTF-8 handoff summary file (required)")
	chatCompactCmd.Flags().String("output", "json", "Output format: table or json")
	chatCmd.AddCommand(chatCompactCmd)
	for _, c := range []*cobra.Command{chatHistoryCmd, chatThreadCmd} {
		c.Flags().Int("limit", 0, "Maximum number of messages to return (the server clamps the range)")
		c.Flags().String("before", "", "Opaque cursor (a next_cursor from a prior page) to read older messages")
		c.Flags().String("output", "json", "Output format: table or json")
	}
	chatCmd.AddCommand(chatHistoryCmd)
	chatCmd.AddCommand(chatThreadCmd)
}

// readChatSummary loads and validates a --summary-file the way the server will.
func readChatSummary(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("--summary-file is required")
	}
	summary, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read summary file: %w", err)
	}
	if !utf8.Valid(summary) || strings.TrimSpace(string(summary)) == "" || len(summary) > 64<<10 {
		return "", fmt.Errorf("summary must be UTF-8, non-empty and at most 65536 bytes")
	}
	return string(summary), nil
}

// serverLacksCompact reports a 404/405 from a server that predates
// /api/chat/compact. The endpoint's own "chat session not found" 404 is not
// skew. Skew never falls back to "continue": that archives the DM.
func serverLacksCompact(err error) bool {
	var httpErr *cli.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	switch httpErr.StatusCode {
	case http.StatusMethodNotAllowed:
		return true
	case http.StatusNotFound:
		return !strings.Contains(httpErr.Body, "chat session not found")
	}
	return false
}

func runChatCompact(cmd *cobra.Command, _ []string) error {
	path, _ := cmd.Flags().GetString("summary-file")
	summary, err := readChatSummary(path)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var response map[string]any
	if err := client.PostJSON(ctx, "/api/chat/compact", map[string]string{"summary": summary}, &response); err != nil {
		if serverLacksCompact(err) {
			return fmt.Errorf("server does not support in-place compaction; DM unchanged")
		}
		return fmt.Errorf("compact chat: %w", err)
	}
	return cli.PrintJSON(cmd.OutOrStdout(), response)
}

func runChatContinue(cmd *cobra.Command, _ []string) error {
	path, _ := cmd.Flags().GetString("summary-file")
	fork, _ := cmd.Flags().GetBool("fork")
	key, _ := cmd.Flags().GetString("idempotency-key")
	endpoint, action := "/api/chat/continue", "continue chat"
	body := map[string]string{}
	switch {
	case fork && strings.TrimSpace(path) != "":
		return fmt.Errorf("fork chat: --summary-file is not accepted with --fork; a fork copies the whole DM")
	case fork:
		endpoint, action = "/api/chat/fork", "fork chat"
		if key != "" {
			body["idempotency_key"] = key
		}
	case key != "":
		return fmt.Errorf("--idempotency-key requires --fork")
	case strings.TrimSpace(path) == "":
		return fmt.Errorf("--summary-file is required")
	default:
		summary, err := readChatSummary(path)
		if err != nil {
			return err
		}
		body["summary"] = summary
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var response map[string]any
	if err := client.PostJSON(ctx, endpoint, body, &response); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "table" {
		fmt.Fprintln(cmd.OutOrStdout(), strVal(response, "url"))
		return nil
	}
	return cli.PrintJSON(cmd.OutOrStdout(), response)
}

func runChatHistory(cmd *cobra.Command, _ []string) error {
	resp, err := fetchChatRead(cmd, "/api/chat/history", "")
	if err != nil {
		return err
	}
	return renderChatRead(cmd, resp, true)
}

func runChatThread(cmd *cobra.Command, args []string) error {
	threadID := ""
	if len(args) == 1 {
		threadID = args[0]
	}
	resp, err := fetchChatRead(cmd, "/api/chat/thread", threadID)
	if err != nil {
		return err
	}
	return renderChatRead(cmd, resp, false)
}

// fetchChatRead builds the request (shared --limit/--before paging, plus the
// optional thread id) and decodes the response.
func fetchChatRead(cmd *cobra.Command, basePath, threadID string) (map[string]any, error) {
	client, err := newAPIClient(cmd)
	if err != nil {
		return nil, err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	limit, _ := cmd.Flags().GetInt("limit")
	before, _ := cmd.Flags().GetString("before")

	q := url.Values{}
	if threadID != "" {
		q.Set("id", threadID)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if before != "" {
		q.Set("before", before)
	}
	path := basePath
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var resp map[string]any
	if err := client.GetJSON(ctx, path, &resp); err != nil {
		return nil, fmt.Errorf("read chat: %w", err)
	}
	return resp, nil
}

// renderChatRead prints the response as JSON (default) or a table. The overview
// table adds the thread columns so the agent can pick a thread_id to drill into.
func renderChatRead(cmd *cobra.Command, resp map[string]any, overview bool) error {
	output, _ := cmd.Flags().GetString("output")
	if output != "table" {
		return cli.PrintJSON(os.Stdout, resp)
	}
	if note := strVal(resp, "note"); note != "" {
		fmt.Fprintln(os.Stdout, note)
		return nil
	}
	msgs, _ := resp["messages"].([]any)
	var headers []string
	if overview {
		headers = []string{"TS", "ROLE", "AUTHOR", "THREAD_ID", "REPLIES", "TEXT"}
	} else {
		headers = []string{"TS", "ROLE", "AUTHOR", "TEXT"}
	}
	rows := make([][]string, 0, len(msgs))
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		if overview {
			rows = append(rows, []string{strVal(m, "ts"), strVal(m, "role"), strVal(m, "author"), strVal(m, "thread_id"), numVal(m, "reply_count"), strVal(m, "text")})
		} else {
			rows = append(rows, []string{strVal(m, "ts"), strVal(m, "role"), strVal(m, "author"), strVal(m, "text")})
		}
	}
	cli.PrintTable(os.Stdout, headers, rows)
	return nil
}

// numVal renders a numeric JSON field as a string, blank when zero/absent.
func numVal(m map[string]any, key string) string {
	if v, ok := m[key].(float64); ok && v != 0 {
		return strconv.Itoa(int(v))
	}
	return ""
}
