package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

const waitingSectionHeader = "## Đang chờ anh"

// openAsk mirrors the open-asks read. It has no answer or quote fields.
type openAsk struct {
	ID               string `json:"id"`
	IssueID          string `json:"issue_id"`
	IssueIdentifier  string `json:"issue_identifier"`
	Ask              string `json:"ask"`
	Class            string `json:"escalation_class"`
	CreatedAt        string `json:"created_at"`
	CardLink         string `json:"card_link"`
	AskingAgentID    string `json:"asking_agent_id"`
	DefaultIfNoReply string `json:"default_if_no_reply"`
}

func runDecisionListOpenAsks(command *cobra.Command, memberID string) error {
	if state, _ := command.Flags().GetString("state"); state != "open" {
		return fmt.Errorf("--for needs --state open")
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var asks []openAsk
	if err := client.GetJSON(ctx, "/api/decisions/open-asks?"+url.Values{"for": {memberID}}.Encode(), &asks); err != nil {
		return fmt.Errorf("list open asks: %w", err)
	}
	if section, _ := command.Flags().GetBool("section"); section {
		olderThanHours, _ := command.Flags().GetInt("older-than-hours")
		maxLines, _ := command.Flags().GetInt("max-lines")
		_, err := fmt.Fprint(os.Stdout, waitingSection(asks, time.Now(), time.Duration(olderThanHours)*time.Hour, maxLines))
		return err
	}
	if output, _ := command.Flags().GetString("output"); output == "table" {
		rows := make([][]string, 0, len(asks))
		for _, ask := range asks {
			rows = append(rows, []string{ask.IssueIdentifier, ask.Class, ask.CreatedAt, ask.Ask})
		}
		cli.PrintTable(os.Stdout, []string{"ISSUE", "CLASS", "CREATED", "ASK"}, rows)
		return nil
	}
	if asks == nil {
		asks = []openAsk{}
	}
	return cli.PrintJSON(os.Stdout, asks)
}

// waitingSection renders the evening brief's "Đang chờ anh" section: asks open
// longer than olderThan, oldest first, one line each. The header and any
// "omitted" line count toward maxLines. default_if_no_reply is shown as text
// only; nothing here applies it.
func waitingSection(asks []openAsk, now time.Time, olderThan time.Duration, maxLines int) string {
	var old []openAsk
	var ages []time.Duration
	for _, ask := range asks {
		created, err := time.Parse(time.RFC3339, ask.CreatedAt)
		if err != nil || now.Sub(created) < olderThan {
			continue
		}
		old = append(old, ask)
		ages = append(ages, now.Sub(created))
	}
	if len(old) == 0 || maxLines < 2 {
		return ""
	}
	shown := len(old)
	if shown > maxLines-1 {
		shown = max(maxLines-2, 0)
	}
	var out strings.Builder
	out.WriteString(waitingSectionHeader + "\n")
	for index := 0; index < shown; index++ {
		ask := old[index]
		fmt.Fprintf(&out, "- [%s](mention://issue/%s) — %s — %dh — mặc định (chỉ mô tả, never applied): %s\n",
			ask.IssueIdentifier, ask.IssueID, oneLine(ask.Ask, 100), int(ages[index].Hours()), oneLine(ask.DefaultIfNoReply, 80))
	}
	if shown < len(old) {
		fmt.Fprintf(&out, "- +%d omitted\n", len(old)-shown)
	}
	return out.String()
}

func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit-1]) + "…"
}
