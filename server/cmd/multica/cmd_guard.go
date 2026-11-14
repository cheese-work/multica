package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/guard"
)

// guardExit ends the process with the guarded tool's exit code. Tests replace it.
var guardExit = os.Exit

var guardCmd = &cobra.Command{
	Use:   "guard",
	Short: "Check agent tool calls against the workspace policy",
}

var guardExecCmd = &cobra.Command{
	Use:   "exec <tool> -- <args...>",
	Short: "Run a tool call through the guard",
	Long: `Run <tool> with <args> after the guard has checked, fixed or refused the call.

Today the guard covers "gh pr create|edit|ready" and writes to pull requests
and issues through "gh api". Shadow check: "multica issue wakeup create|update"
with --filter-actor-type member logs a hit when the issue has no open decision
request for that member after the last answered one; the call always runs and
nothing is printed. Every other call runs unchanged.

Environment:
  MULTICA_GUARD_POLICY, MULTICA_GUARD_POLICY_SHA256  signed policy file and its digest
  MULTICA_GUARD_LOG                                  append one JSON line per decision
  MULTICA_GUARD_REAL_<TOOL>                          path of the real binary (for example MULTICA_GUARD_REAL_GH)

Exit code: the tool's own, 2 when the guard refuses the call.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runGuardExec,
}

var guardCheckPRCmd = &cobra.Command{
	Use:   "check-pr <pr-url|number|branch>",
	Short: "Check one pull request against the guard rules without changing it",
	Long: `Read a pull request back and check it against the workspace policy and the
repository's own rules in .github/agent-rules.yml, read from the current tip of
the PR's protected base branch (never from the PR branch). Nothing is edited.
Use it as a CI check; it reads the rules again on every run.

Exit code: 0 when the PR passes, 1 when it breaks a rule, 2 when the guard
refuses (a malformed rules file, an unreadable base branch, a locked-down
policy).`,
	Args: cobra.ExactArgs(1),
	RunE: runGuardCheckPR,
}

func init() {
	guardCheckPRCmd.Flags().StringP("repo", "R", "", "Repository of the pull request, owner/name")
	guardCmd.AddCommand(guardExecCmd, guardCheckPRCmd)
}

func runGuardCheckPR(cmd *cobra.Command, args []string) error {
	pol, warn := guard.PolicyFromEnv(os.Getenv)
	if warn != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), warn)
	}
	sel := args
	if r, _ := cmd.Flags().GetString("repo"); r != "" {
		sel = append(sel, "--repo", r)
	}
	e := &guard.Engine{
		Policy: pol,
		Out:    cmd.OutOrStdout(),
		Err:    cmd.ErrOrStderr(),
		Runner: guard.ExecRunner{
			Real:   func(string) string { return os.Getenv("MULTICA_GUARD_REAL_GH") },
			Stdin:  cmd.InOrStdin(),
			Stdout: cmd.OutOrStdout(),
			Stderr: cmd.ErrOrStderr(),
		},
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	code, err := e.CheckPR(ctx, sel)
	if err != nil {
		return fmt.Errorf("guard: check-pr: %w", err)
	}
	if code != 0 {
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		guardExit(code)
	}
	return nil
}

func runGuardExec(cmd *cobra.Command, args []string) error {
	tool, rest := args[0], args[1:]
	pol, warn := guard.PolicyFromEnv(os.Getenv)
	if warn != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), warn)
	}

	var logw io.Writer
	if p := os.Getenv(guard.LogEnv); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "guard: decision log disabled: %v\n", err)
		} else {
			defer f.Close()
			logw = f
		}
	}

	realEnv := "MULTICA_GUARD_REAL_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(tool))
	e := &guard.Engine{
		Policy: pol,
		Log:    logw,
		Out:    cmd.OutOrStdout(),
		Err:    cmd.ErrOrStderr(),
		Runner: guard.ExecRunner{
			Real:   func(string) string { return os.Getenv(realEnv) },
			Stdin:  cmd.InOrStdin(),
			Stdout: cmd.OutOrStdout(),
			Stderr: cmd.ErrOrStderr(),
		},
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	code, err := e.Exec(ctx, tool, rest)
	if err != nil {
		return fmt.Errorf("guard: %s: %w", tool, err)
	}
	if code != 0 {
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		guardExit(code)
	}
	return nil
}
