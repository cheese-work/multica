package main

import (
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/util"
)

func guardCommentSelfMention(content, agentID string) error {
	if agentID == "" {
		return nil
	}
	for _, mention := range util.ParseMentions(strings.ReplaceAll(content, "\x00", "")) {
		if mention.Type == "agent" && strings.EqualFold(mention.ID, agentID) {
			return fmt.Errorf("agent comment self-mention is not allowed: write your own name as plain text instead of mention://agent/%s", agentID)
		}
	}
	return nil
}
