package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatPromptHasNoLaneSummary(test *testing.T) {
	for _, issueID := range []string{"", "issue"} {
		test.Run("issue="+issueID, func(test *testing.T) {
			var task Task
			if err := json.Unmarshal([]byte(`{"chat_session_id":"chat","chat_lane_summary_enabled":true,"chat_lane_summary":"Legacy private lane","chat_lane_summary_updated_at":"2026-10-13T00:00:00Z","chat_lane_summary_message_count":20}`), &task); err != nil {
				test.Fatal(err)
			}
			task.IssueID = issueID
			prompt := buildChatPrompt(task)
			for _, forbidden := range []string{"DM lane summary", "Legacy private lane", "chat summary set", "messages have arrived since the last summary update"} {
				if strings.Contains(prompt, forbidden) {
					test.Fatalf("DM prompt contains removed lane-summary content %q", forbidden)
				}
			}
		})
	}
}
