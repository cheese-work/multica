package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/guard"
)

// The workspace tool-guard policy lives in settings.guard_policy (CHE-5204).
// Reads go straight to the workspace; a change is only ever proposed here: it
// is a CHE-5167 settings patch that a person approves through a decision.

var guardPluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "Read and propose changes to the workspace guard plugins and scope ladder",
}

var guardPluginListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the plugins of every scope in the stored policy",
	Args:  cobra.NoArgs,
	RunE:  runGuardPluginList,
}

var guardPluginGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Show every scope's definition of one plugin",
	Args:  cobra.ExactArgs(1),
	RunE:  runGuardPluginGet,
}

var guardPluginSetCmd = &cobra.Command{
	Use:   "set --file <policy.json>",
	Short: "Write the decision action file that replaces the stored guard policy",
	Long: `Validate a policy document (workspace plugins, review rules, and org, repo and
path layers; a layer or the workspace may set "locked") and write the action
file for "multica decision request --action-file". Nothing is sent and the
stored policy does not change until the decision is approved. Use --dry-run to
print both digests and the action.

Deeper layers override shallower ones key by key (plugin:<name>, review:<repo>,
verify). A locked layer freezes the keys it sets against deeper layers.`,
	Args: cobra.NoArgs,
	RunE: runGuardPluginSet,
}

func init() {
	guardPluginSetCmd.Flags().String("file", "", "Policy document (JSON)")
	guardPluginSetCmd.Flags().String("out", "", "Where to write the action file")
	guardPluginSetCmd.Flags().Bool("dry-run", false, "Print both digests and the action; write nothing")
	guardPluginSetCmd.Flags().Int("expires-in", 0, "Seconds the request stays open (0 = server default)")
	guardPluginCmd.AddCommand(guardPluginListCmd, guardPluginGetCmd, guardPluginSetCmd)
	guardCmd.AddCommand(guardPluginCmd)
}

// guardPluginRow is one plugin as one scope defines it.
type guardPluginRow struct {
	Scope  string `json:"scope"`
	Target string `json:"target,omitempty"`
	Name   string `json:"name"`
	Action string `json:"action"`
	Locked bool   `json:"locked,omitempty"`
}

func guardPluginRows(p *guard.Policy) []guardPluginRow {
	rows := []guardPluginRow{}
	for _, pl := range p.Plugins {
		rows = append(rows, guardPluginRow{Scope: "workspace", Name: pl.Name, Action: pl.Action, Locked: p.Locked})
	}
	for _, l := range p.Layers {
		target := l.Org
		switch l.Scope {
		case guard.ScopeRepo:
			target = l.Repo
		case guard.ScopePath:
			target = l.Repo + ":" + l.Path
		}
		for _, pl := range l.Plugins {
			rows = append(rows, guardPluginRow{Scope: l.Scope, Target: target, Name: pl.Name, Action: pl.Action, Locked: l.Locked})
		}
	}
	return rows
}

// liveGuardPolicy reads settings.guard_policy; a workspace without one reads as
// an empty policy.
func liveGuardPolicy(cmd *cobra.Command) (*guard.Policy, error) {
	client, err := newAPIClient(cmd)
	if err != nil {
		return nil, err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	live, err := getLive(ctx, client, "/api/workspaces/"+url.PathEscape(client.WorkspaceID))
	if err != nil {
		return nil, err
	}
	var settings map[string]json.RawMessage
	if len(live["settings"]) > 0 {
		_ = json.Unmarshal(live["settings"], &settings)
	}
	if isJSONNull(settings["guard_policy"]) {
		return &guard.Policy{Version: 1}, nil
	}
	return guard.ParsePolicy(settings["guard_policy"])
}

func printGuardRows(cmd *cobra.Command, rows []guardPluginRow) error {
	if out, _ := cmd.Flags().GetString("output"); out == "table" {
		table := make([][]string, 0, len(rows))
		for _, r := range rows {
			table = append(table, []string{r.Scope, r.Target, r.Name, r.Action, fmt.Sprint(r.Locked)})
		}
		cli.PrintTable(os.Stdout, []string{"SCOPE", "TARGET", "PLUGIN", "ACTION", "LOCKED"}, table)
		return nil
	}
	return cli.PrintJSON(os.Stdout, rows)
}

func runGuardPluginList(cmd *cobra.Command, _ []string) error {
	p, err := liveGuardPolicy(cmd)
	if err != nil {
		return err
	}
	return printGuardRows(cmd, guardPluginRows(p))
}

func runGuardPluginGet(cmd *cobra.Command, args []string) error {
	p, err := liveGuardPolicy(cmd)
	if err != nil {
		return err
	}
	rows := []guardPluginRow{}
	for _, r := range guardPluginRows(p) {
		if r.Name == args[0] {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return fmt.Errorf("no plugin %q in the stored policy", args[0])
	}
	return printGuardRows(cmd, rows)
}

func runGuardPluginSet(cmd *cobra.Command, _ []string) error {
	file, _ := cmd.Flags().GetString("file")
	out, _ := cmd.Flags().GetString("out")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	expires, _ := cmd.Flags().GetInt("expires-in")
	if file == "" {
		return errors.New("--file is required")
	}
	if !dryRun && out == "" {
		return errors.New("--out is required (or use --dry-run)")
	}
	text, err := readDecisionText(file, "policy")
	if err != nil {
		return err
	}
	// Refuse what the daemon's guard would refuse, before any server read.
	if _, err := guard.ParsePolicy([]byte(text)); err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(text)); err != nil {
		return err
	}
	patch := map[string]json.RawMessage{"guard_policy": compact.Bytes()}
	if err := checkProposeField("workspace.settings", patch); err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	p, err := settingsProposal(ctx, client, patch)
	if err != nil {
		return err
	}
	return emitProposal(p, expires, out, dryRun)
}
