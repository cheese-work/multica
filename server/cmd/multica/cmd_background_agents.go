package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/cli"
)

// Background-agents commands (CHE-4115). A job (dm_title, quick_actions,
// compact, routing) maps to a primary and a fallback agent. The workspace
// command edits the workspace default; `me` edits the caller's own override.
// Both are human-only writes: the server answers a machine actor with 403 and a
// stable code, which this file prints.

const (
	bgSourceUser      = "user"
	bgSourceWorkspace = "workspace"
	bgSourceOff       = "off"
)

// bgScope is one of the two settings the commands edit.
type bgScope struct {
	label string // "workspace" or "me"
	path  string // path below /api/workspaces/{id}
	user  bool   // user-scoped: the catalog without workspace-only jobs
}

var (
	bgWorkspaceScope = bgScope{"workspace", "/background-agents", false}
	bgMeScope        = bgScope{"me", "/me/background-agents", true}
)

var workspaceBackgroundAgentsCmd = &cobra.Command{
	Use:   "background-agents",
	Short: "Show or set the agent chain of each background job (workspace default)",
	Long: "A background job runs as a task of a primary agent, then a fallback agent. " +
		"These commands show and set the workspace default; members override it " +
		"with `multica me background-agents`. Writes are owner/admin only and " +
		"refused for task tokens.",
}

var workspaceBackgroundAgentsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List each job's workspace chain",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return runBackgroundAgentsList(cmd, bgWorkspaceScope) },
}

var workspaceBackgroundAgentsSetCmd = &cobra.Command{
	Use:   "set <job>",
	Short: "Set a job's workspace agent and/or fallback agent",
	Example: "  multica workspace background-agents set dm_title --agent haiku --fallback luna\n" +
		"  multica workspace background-agents set quick_actions --agent none",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBackgroundAgentsSet(cmd, args[0], bgWorkspaceScope)
	},
}

var meCmd = &cobra.Command{
	Use:   "me",
	Short: "Your own settings in the current workspace",
}

var meBackgroundAgentsCmd = &cobra.Command{
	Use:   "background-agents",
	Short: "Show or set your own agent chain of each background job",
	Long: "Your chain wins over the workspace default for the jobs you own " +
		"(dm_title, quick_actions, compact). A job with no chain of yours uses the " +
		"workspace default.",
}

var meBackgroundAgentsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List each job's effective chain and where it comes from (user, workspace or off)",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return runBackgroundAgentsList(cmd, bgMeScope) },
}

var meBackgroundAgentsSetCmd = &cobra.Command{
	Use:   "set <job>",
	Short: "Set your own agent and/or fallback agent for a job",
	Example: "  multica me background-agents set dm_title --agent haiku\n" +
		"  multica me background-agents set dm_title --agent none   # back to the workspace default",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error { return runBackgroundAgentsSet(cmd, args[0], bgMeScope) },
}

func init() {
	workspaceCmd.AddCommand(workspaceBackgroundAgentsCmd)
	workspaceBackgroundAgentsCmd.AddCommand(workspaceBackgroundAgentsListCmd, workspaceBackgroundAgentsSetCmd)
	meCmd.AddCommand(meBackgroundAgentsCmd)
	meBackgroundAgentsCmd.AddCommand(meBackgroundAgentsListCmd, meBackgroundAgentsSetCmd)

	for _, c := range []*cobra.Command{workspaceBackgroundAgentsSetCmd, meBackgroundAgentsSetCmd} {
		c.Flags().String("agent", "", "Primary agent, by id or name in the workspace; none clears it")
		c.Flags().String("fallback", "", "Fallback agent, by id or name in the workspace; none clears it")
	}
	for _, c := range []*cobra.Command{workspaceBackgroundAgentsListCmd, workspaceBackgroundAgentsSetCmd, meBackgroundAgentsListCmd, meBackgroundAgentsSetCmd} {
		c.Flags().String("output", "table", "Output format: table or json")
	}
}

// bgJobs is the catalog in a fixed order, minus workspace-only jobs for the
// user scope.
func bgJobs(s bgScope) []string {
	jobs := make([]string, 0, len(bgagents.Catalog))
	for job, spec := range bgagents.Catalog {
		if !s.user || spec.UserScoped {
			jobs = append(jobs, string(job))
		}
	}
	slices.Sort(jobs)
	return jobs
}

func bgCheckJob(s bgScope, job string) error {
	if !slices.Contains(bgJobs(s), job) {
		return fmt.Errorf("unknown job %q for %s scope; valid jobs: %s", job, s.label, strings.Join(bgJobs(s), ", "))
	}
	return nil
}

type bgChain = map[string]any

// bgRead returns a scope's stored value: job -> chain.
func bgRead(ctx context.Context, client *cli.APIClient, wsID string, s bgScope) (map[string]bgChain, error) {
	var out map[string]bgChain
	if err := client.GetJSON(ctx, "/api/workspaces/"+wsID+s.path, &out); err != nil {
		return nil, fmt.Errorf("read background agents: %w", err)
	}
	if out == nil {
		out = map[string]bgChain{}
	}
	return out, nil
}

func bgHasAgent(c bgChain) bool { return strVal(c, "agent_id") != "" }

// bgView is what list prints: every job of the scope with its effective chain
// and source. For `me` the user chain wins when it names a primary agent, as the
// server's resolver does; eligibility is not evaluated here.
func bgView(s bgScope, workspace, mine map[string]bgChain) map[string]bgChain {
	view := map[string]bgChain{}
	for _, job := range bgJobs(s) {
		entry, source := bgChain{}, bgSourceOff
		switch {
		case s.user && bgHasAgent(mine[job]):
			entry, source = mine[job], bgSourceUser
		case bgHasAgent(workspace[job]):
			entry, source = workspace[job], bgSourceWorkspace
		}
		out := bgChain{"source": source}
		for k, v := range entry {
			out[k] = v
		}
		view[job] = out
	}
	return view
}

func bgLoadView(ctx context.Context, client *cli.APIClient, wsID string, s bgScope) (map[string]bgChain, error) {
	workspace, err := bgRead(ctx, client, wsID, bgWorkspaceScope)
	if err != nil {
		return nil, err
	}
	var mine map[string]bgChain
	if s.user {
		if mine, err = bgRead(ctx, client, wsID, bgMeScope); err != nil {
			return nil, err
		}
	}
	return bgView(s, workspace, mine), nil
}

func bgPrint(cmd *cobra.Command, wsID string, s bgScope, view map[string]bgChain) error {
	if out, _ := cmd.Flags().GetString("output"); out == "json" {
		return cli.PrintJSON(os.Stdout, map[string]any{"workspace_id": wsID, "scope": s.label, "jobs": view})
	}
	rows := make([][]string, 0, len(view))
	for _, job := range bgJobs(s) {
		c := view[job]
		rows = append(rows, []string{job, orDash(strVal(c, "agent_id")), orDash(strVal(c, "fallback_agent_id")), strVal(c, "source")})
	}
	cli.PrintTable(os.Stdout, []string{"JOB", "AGENT", "FALLBACK", "SOURCE"}, rows)
	return nil
}

func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

func runBackgroundAgentsList(cmd *cobra.Command, s bgScope) error {
	wsID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	view, err := bgLoadView(ctx, client, wsID, s)
	if err != nil {
		return err
	}
	return bgPrint(cmd, wsID, s, view)
}

// bgResolveFlag turns --agent / --fallback into an id, or "" for none.
func bgResolveFlag(ctx context.Context, cmd *cobra.Command, client *cli.APIClient, flag string) (id string, set bool, err error) {
	if !cmd.Flags().Changed(flag) {
		return "", false, nil
	}
	v, _ := cmd.Flags().GetString(flag)
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", false, fmt.Errorf("--%s needs an agent id, an agent name or none", flag)
	case v == "none":
		return "", true, nil
	}
	id, err = resolveAgent(ctx, client, v)
	return id, true, err
}

func runBackgroundAgentsSet(cmd *cobra.Command, job string, s bgScope) error {
	if err := bgCheckJob(s, job); err != nil {
		return err
	}
	if !cmd.Flags().Changed("agent") && !cmd.Flags().Changed("fallback") {
		return errors.New("nothing to set: pass --agent and/or --fallback (an id, a name, or none to clear)")
	}
	wsID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	fields := map[string]*string{} // nil clears the field
	for flag, field := range map[string]string{"agent": "agent_id", "fallback": "fallback_agent_id"} {
		id, set, err := bgResolveFlag(ctx, cmd, client, flag)
		if err != nil {
			return err
		}
		if !set {
			continue
		}
		if id == "" {
			fields[field] = nil
		} else {
			fields[field] = &id
		}
	}

	body, err := bgWriteBody(ctx, client, wsID, s, job, fields)
	if err != nil {
		return err
	}
	if err := client.PutJSON(ctx, "/api/workspaces/"+wsID+s.path, body, nil); err != nil {
		return bgWriteError("set background agents", err)
	}
	view, err := bgLoadView(ctx, client, wsID, s)
	if err != nil {
		return err
	}
	return bgPrint(cmd, wsID, s, view)
}

// bgWriteBody builds the PUT body. The personal route merges per field, so a
// cleared field is sent as null. The workspace route replaces the whole value,
// so the current value goes back with the job's fields changed; a cleared field
// is left out.
func bgWriteBody(ctx context.Context, client *cli.APIClient, wsID string, s bgScope, job string, fields map[string]*string) (map[string]any, error) {
	if s.user {
		patch := map[string]any{}
		for field, id := range fields {
			if id == nil {
				patch[field] = nil
			} else {
				patch[field] = *id
			}
		}
		return map[string]any{job: patch}, nil
	}
	current, err := bgRead(ctx, client, wsID, s)
	if err != nil {
		return nil, err
	}
	next := map[string]any{}
	for j, c := range current {
		next[j] = c
	}
	chain := bgChain{}
	for k, v := range current[job] {
		chain[k] = v
	}
	for field, id := range fields {
		if id == nil {
			delete(chain, field)
		} else {
			chain[field] = *id
		}
	}
	if len(chain) == 0 {
		delete(next, job)
	} else {
		next[job] = chain
	}
	return next, nil
}

// bgWriteError keeps the typed error (exit code) and puts the server's stable
// code and message in front of the user.
func bgWriteError(op string, err error) error {
	code := cli.ServerErrorCode(err)
	if code == "" {
		return fmt.Errorf("%s: %w", op, err)
	}
	var httpErr *cli.HTTPError
	var body struct {
		Error string `json:"error"`
	}
	if errors.As(err, &httpErr) {
		_ = json.Unmarshal([]byte(httpErr.Body), &body)
	}
	return cli.WithUserMessage(fmt.Sprintf("%s: %s: %s", op, code, body.Error), err)
}
