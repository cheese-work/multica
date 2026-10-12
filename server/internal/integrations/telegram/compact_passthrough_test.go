package telegram

import "testing"

func TestWebCompactCommandRemainsLiteralTelegramInput(t *testing.T) {
	for _, content := range []string{"/compact", "/compact keep evidence"} {
		update := Update{UpdateID: 42, Message: &Message{MessageID: 7, From: &User{ID: 111}, Chat: Chat{ID: 555, Type: "private"}, Text: content}}
		message, ok := inboundFromUpdate(update, 999, "my_bot")
		if !ok || message.Text != content {
			t.Errorf("input %q: accepted=%v, text=%q", content, ok, message.Text)
		}
	}
}
