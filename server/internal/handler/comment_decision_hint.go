package handler

import (
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func commentDecisionHints(comment db.Comment, decisionIntent bool) []string {
	if comment.AuthorType != "agent" || decisionIntent {
		return nil
	}
	var commands []string
	for _, mention := range util.ParseMentions(comment.Content) {
		if mention.Type != "member" {
			continue
		}
		humanID, err := util.ParseUUID(mention.ID)
		if err != nil {
			continue
		}
		commands = append(commands, fmt.Sprintf("multica decision request --issue %s --for %s --class CLASS --ask-file ASK.json", uuidToString(comment.IssueID), uuidToString(humanID)))
	}
	if len(commands) == 0 {
		return nil
	}
	return []string{"If this needs a human answer, prefer a decision request. Use: " + strings.Join(commands, "; ") + ". A plain mention is for FYI."}
}
