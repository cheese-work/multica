package handler

import (
	"encoding/json"
	"testing"
)

func TestChatResponsesHaveNoLaneSummary(test *testing.T) {
	for _, response := range []any{&ChatSessionResponse{}, &AgentTaskResponse{}} {
		if err := json.Unmarshal([]byte(`{"lane_summary":"Legacy private lane","lane_summary_updated_at":"2026-10-13T00:00:00Z","chat_lane_summary_enabled":true,"chat_lane_summary":"Legacy private lane","chat_lane_summary_updated_at":"2026-10-13T00:00:00Z","chat_lane_summary_message_count":20}`), response); err != nil {
			test.Fatal(err)
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			test.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			test.Fatal(err)
		}
		for _, forbidden := range []string{"lane_summary", "lane_summary_updated_at", "chat_lane_summary_enabled", "chat_lane_summary", "chat_lane_summary_updated_at", "chat_lane_summary_message_count"} {
			if _, exists := fields[forbidden]; exists {
				test.Fatalf("%T still exposes %s", response, forbidden)
			}
		}
	}
}
