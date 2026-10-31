package execenv

import (
	"strings"
	"testing"
)

// TestAvailableCommandsKeepsForkLines pins the fork's own Available Commands
// lines through upstream brief compression (MUL-8292): the `--no-start`
// contract on update/assign (CHE-2070) and the `--until-gh-run` wakeup
// condition with its subscription caveat (CHE-2157). Every kind and provider
// is checked because the section is shared verbatim across them.
func TestAvailableCommandsKeepsForkLines(t *testing.T) {
	t.Parallel()
	for kind, ctx := range availableCommandsKinds {
		for _, provider := range []string{"claude", "codex"} {
			kind, ctx, provider := kind, ctx, provider
			t.Run(kind+"/"+provider, func(t *testing.T) {
				t.Parallel()
				ac := availableCommandsSection(t, buildMetaSkillContent(provider, ctx))
				for _, want := range []string{
					"[--due-date <YYYY-MM-DD>] [--no-start]` — update fields",
					"`multica issue assign <id> (--to X | --to-id <uuid> | --unassign) [--no-start]` — change ownership. On assign/update/status, `--no-start` records the change without starting another run — use it when the work is already underway.",
					"`--until-gh-run owner/repo:RUN_ID --until-gh-run-state completed|waiting` for a GitHub Actions run",
					"`--until-gh-run` sees only GitHub events received after the GitHub App subscribed, so add `--expires-in D --on-timeout wake`.",
				} {
					if !strings.Contains(ac, want) {
						t.Errorf("Available Commands lost fork line %q\n---\n%s", want, ac)
					}
				}
			})
		}
	}
}
