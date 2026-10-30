package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func openAsksAt(now time.Time, count int) []openAsk {
	asks := make([]openAsk, 0, count)
	for index := 0; index < count; index++ {
		asks = append(asks, openAsk{
			IssueID: fmt.Sprintf("uuid-%d", index), IssueIdentifier: fmt.Sprintf("CHE-%d", index),
			Ask: "Ask " + fmt.Sprint(index), CreatedAt: now.Add(-time.Duration(48-index) * time.Hour).Format(time.RFC3339),
			DefaultIfNoReply: "Keep the plan",
		})
	}
	return asks
}

func TestWaitingSectionStaysWithinLineBudget(test *testing.T) {
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	asks := openAsksAt(now, 8)
	asks = append(asks, openAsk{IssueID: "fresh", IssueIdentifier: "CHE-99", Ask: "Fresh", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339)})
	for _, budget := range []int{2, 3, 7, 30} {
		section := waitingSection(asks, now, 24*time.Hour, budget)
		lines := strings.Split(strings.TrimSuffix(section, "\n"), "\n")
		if len(lines) > budget {
			test.Fatalf("budget %d: %d lines\n%s", budget, len(lines), section)
		}
		if lines[0] != "## Đang chờ anh" {
			test.Fatalf("budget %d: header=%q", budget, lines[0])
		}
		if strings.Contains(section, "CHE-99") {
			test.Fatalf("an ask younger than N appeared:\n%s", section)
		}
		if budget < 30 && !strings.Contains(lines[len(lines)-1], "omitted") {
			test.Fatalf("budget %d: shrunk section has no omitted count:\n%s", budget, section)
		}
	}
	section := waitingSection(asks, now, 24*time.Hour, 7)
	if !strings.Contains(section, "+3 omitted") || strings.Index(section, "CHE-0") > strings.Index(section, "CHE-1") {
		test.Fatalf("want oldest first and 3 omitted:\n%s", section)
	}
	if !strings.Contains(section, "mention://issue/uuid-0") || !strings.Contains(section, "Keep the plan") || !strings.Contains(section, "never applied") {
		test.Fatalf("line lacks link, default or no-apply note:\n%s", section)
	}
	if got := waitingSection(asks, now, 24*time.Hour, 1); got != "" {
		test.Fatalf("budget 1 cannot hold header plus count, got %q", got)
	}
	if got := waitingSection(nil, now, 24*time.Hour, 30); got != "" {
		test.Fatalf("no old asks must print nothing, got %q", got)
	}
}
