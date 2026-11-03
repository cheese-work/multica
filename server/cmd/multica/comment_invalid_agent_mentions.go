package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/cli"
)

// invalidAgentMentionsMessage decodes the 422 invalid_agent_mentions body the
// server returns when a comment links an agent that does not exist, is not
// invocable by the caller, or is archived. The server sends byte offsets only
// (no id or name, so unknown and private look alike); the text of each range
// is cut from sent, the content this CLI itself posted. An offset pair that
// does not fit sent, or splits a UTF-8 sequence, prints the range alone.
func invalidAgentMentionsMessage(httpErr *cli.HTTPError, sent string) (string, bool) {
	if httpErr == nil || httpErr.StatusCode != http.StatusUnprocessableEntity {
		return "", false
	}
	var payload struct {
		Code            string `json:"code"`
		InvalidMentions []struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"invalid_mentions"`
	}
	if json.Unmarshal([]byte(httpErr.Body), &payload) != nil || payload.Code != "invalid_agent_mentions" {
		return "", false
	}
	var message strings.Builder
	message.WriteString("comment rejected (invalid_agent_mentions): the comment was not saved " +
		"(attachments already uploaded stay on the issue). " +
		"These agent mentions do not exist, are archived, or you cannot invoke them:")
	for _, mention := range payload.InvalidMentions {
		fmt.Fprintf(&message, "\n  bytes %d-%d", mention.Start, mention.End)
		if mention.Start >= 0 && mention.Start <= mention.End && mention.End <= len(sent) &&
			utf8.ValidString(sent[mention.Start:mention.End]) {
			fmt.Fprintf(&message, ": %s", sent[mention.Start:mention.End])
		}
	}
	message.WriteString("\nFix or remove the mention and post again; it is not retried automatically.")
	return message.String(), true
}

// invalidAgentMentionsError turns a refused comment write into the failure the
// caller returns. Under --output json the server body goes to stdout unchanged
// so a script can parse it; the readable message goes to stderr either way. It
// returns err untouched when the failure is something else.
func invalidAgentMentionsError(err error, sent, output string, stdout io.Writer) error {
	var httpErr *cli.HTTPError
	if !errors.As(err, &httpErr) {
		return err
	}
	message, ok := invalidAgentMentionsMessage(httpErr, sent)
	if !ok {
		return err
	}
	if output != "table" {
		fmt.Fprintln(stdout, httpErr.Body)
	}
	return cli.WithUserMessage(message, err)
}
