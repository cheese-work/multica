package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// --blocked-on (CHE-6972): a structured blocker for a move to blocked. Each
// form is the stored target of a wakeup condition flag. The server validates
// the ref against the workspace (handler/issue_blocked_on.go); this side
// refuses a malformed ref early and turns an issue key into its id.
const blockedOnFormsHelp = "  issue:<id>[@done|ended|in_review|blocked]   (--until-issue, --until-issue-state)\n" +
	"  pr:<checks|merged|conflicts|dequeued>        (--until-pr)\n" +
	"  children[@<stage>]                           (--until-children-done, --stage)\n" +
	"  gh-run:<owner>/<repo>:<run-id>               (--until-gh-run)\n" +
	"  decision:<request-id>                        (a wakeup bound with --decision)\n"

var (
	blockedOnIssueFlagRE = regexp.MustCompile(`^issue:([^@\s]+)(@(?:done|ended|in_review|blocked))?$`)
	blockedOnFlagRE      = regexp.MustCompile(`^(?:pr:(?:checks|merged|conflicts|dequeued)|children(?:@[1-9][0-9]{0,8})?|gh-run:[^\s:]+/[^\s:]+:[1-9][0-9]{0,17}|decision:\S+)$`)
)

// parseBlockedOnFlag returns the trimmed --blocked-on ref, or "" when the flag
// is unset. The flag is refused on any status but blocked.
func parseBlockedOnFlag(cmd *cobra.Command, status string) (string, error) {
	if !cmd.Flags().Changed("blocked-on") {
		return "", nil
	}
	ref, _ := cmd.Flags().GetString("blocked-on")
	ref = strings.TrimSpace(ref)
	if status != "blocked" {
		return "", fmt.Errorf("--blocked-on is only accepted with status blocked, not %q", status)
	}
	if !blockedOnIssueFlagRE.MatchString(ref) && !blockedOnFlagRE.MatchString(ref) {
		return "", fmt.Errorf("--blocked-on %q is not a ref. Forms:\n%s", ref, blockedOnFormsHelp)
	}
	return ref, nil
}

// resolveBlockedOnIssue replaces the issue key in an issue:<key> ref with the
// issue id the server stores; other forms pass through.
func resolveBlockedOnIssue(ctx context.Context, client *cli.APIClient, ref string) (string, error) {
	m := blockedOnIssueFlagRE.FindStringSubmatch(ref)
	if m == nil {
		return ref, nil
	}
	issue, err := resolveIssueRef(ctx, client, m[1])
	if err != nil {
		return "", fmt.Errorf("resolve --blocked-on issue: %w", err)
	}
	return "issue:" + issue.ID + m[2], nil
}
