package main

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// audienceFixLine is the one fix line the comment and status help share. The
// server refuses or counts a waiting comment or move that names no audience
// (S4 CHE-7289, S5 CHE-7290); the S2 --notify-only route stays valid.
const audienceFixLine = "When an issue waits (in_review, blocked, need_decision), say who acts next: " +
	"comment with --ask <member-id> if a person acts next or --no-ask if no one does " +
	"(a member mention, --notify-only or multica decision request also counts); " +
	"move with --waiting-on member:<member-id> or --waiting-on none."

// audienceFlags returns the --ask id and --no-ask choice for a comment, or an
// error for a combination the server would refuse. Membership is checked by
// the server; this side refuses malformed input before any request.
func audienceFlags(cmd *cobra.Command, typedKind bool) (ask string, noAsk bool, err error) {
	ask, _ = cmd.Flags().GetString("ask")
	noAsk, _ = cmd.Flags().GetBool("no-ask")
	askSet := cmd.Flags().Changed("ask")
	if !askSet && !noAsk {
		return "", false, nil
	}
	ask = strings.TrimSpace(ask)
	switch {
	case askSet && noAsk:
		return "", false, fmt.Errorf("--ask and --no-ask cannot combine: name who acts next, or say no one does")
	case askSet && ask == "":
		return "", false, fmt.Errorf("--ask requires a member id")
	case askSet && !isUUID(ask):
		return "", false, fmt.Errorf("--ask %q is not a member id (a UUID)", ask)
	case typedKind:
		return "", false, fmt.Errorf("--ask and --no-ask cannot combine with --notify-only, --decision-intent, --decision-record or --decision-veto: those comments are already typed")
	}
	return ask, noAsk, nil
}

// parseWaitingOnFlag returns the trimmed --waiting-on value ("none" or
// "member:<uuid>"), or "" when the flag is unset. Only a move into a waiting
// status takes it.
func parseWaitingOnFlag(cmd *cobra.Command, status string) (string, error) {
	if !cmd.Flags().Changed("waiting-on") {
		return "", nil
	}
	value, _ := cmd.Flags().GetString("waiting-on")
	value = strings.TrimSpace(value)
	if status != "in_review" && status != "blocked" && status != "need_decision" {
		return "", fmt.Errorf("--waiting-on is only accepted with status in_review, blocked or need_decision, not %q", status)
	}
	if id, ok := strings.CutPrefix(value, "member:"); value != "none" && !(ok && isUUID(id)) {
		return "", fmt.Errorf("--waiting-on %q must be member:<member-id> or none", value)
	}
	return value, nil
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
