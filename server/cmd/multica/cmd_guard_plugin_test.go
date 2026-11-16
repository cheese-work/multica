package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

const storedGuardPolicy = `{"version":1,
 "plugins":[{"name":"gh-api","match":{"tool":"gh","subcommand":["api"]},"action":"deny","message":"no api writes"}],
 "locked":true,
 "layers":[
  {"scope":"org","org":"acme","plugins":[{"name":"gh-api","match":{"tool":"gh","subcommand":["api"]},"action":"pass"},{"name":"extra","match":{"tool":"gh"},"action":"deny","message":"x"}]}]}`

func guardPluginServer(t *testing.T, policy string) {
	t.Helper()
	settings := map[string]any{"compact_prompt": "keep"}
	if policy != "" {
		settings["guard_policy"] = json.RawMessage(policy)
	}
	proposeServer(t, map[string]any{"/api/workspaces/ws-1": map[string]any{"settings": settings}})
}

func guardPluginCmdWith(flags map[string]string) *cobra.Command {
	c := &cobra.Command{Use: "plugin"}
	c.Flags().String("file", "", "")
	c.Flags().String("out", "", "")
	c.Flags().Bool("dry-run", false, "")
	c.Flags().Int("expires-in", 0, "")
	c.Flags().String("output", "json", "")
	for k, v := range flags {
		_ = c.Flags().Set(k, v)
	}
	return c
}

func TestGuardPluginListShowsEveryScopeAndLock(t *testing.T) {
	guardPluginServer(t, storedGuardPolicy)
	out, err := captureStdout(t, func() error { return runGuardPluginList(guardPluginCmdWith(nil), nil) })
	if err != nil {
		t.Fatal(err)
	}
	var rows []guardPluginRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v in %q", err, out)
	}
	if len(rows) != 3 || rows[0].Scope != "workspace" || !rows[0].Locked || rows[0].Name != "gh-api" || rows[0].Action != "deny" ||
		rows[1].Scope != "org" || rows[1].Target != "acme" || rows[1].Action != "pass" || rows[2].Name != "extra" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestGuardPluginListWithoutPolicy(t *testing.T) {
	guardPluginServer(t, "")
	out, err := captureStdout(t, func() error { return runGuardPluginList(guardPluginCmdWith(nil), nil) })
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("out %q err %v, want []", out, err)
	}
}

func TestGuardPluginGetReturnsEachDefinition(t *testing.T) {
	guardPluginServer(t, storedGuardPolicy)
	out, err := captureStdout(t, func() error { return runGuardPluginGet(guardPluginCmdWith(nil), []string{"gh-api"}) })
	if err != nil {
		t.Fatal(err)
	}
	var rows []guardPluginRow
	_ = json.Unmarshal([]byte(out), &rows)
	if len(rows) != 2 || rows[0].Scope != "workspace" || rows[1].Scope != "org" {
		t.Fatalf("rows = %+v", rows)
	}
	if _, err := captureStdout(t, func() error { return runGuardPluginGet(guardPluginCmdWith(nil), []string{"nope"}) }); err == nil {
		t.Fatal("unknown plugin must be an error")
	}
}

func TestGuardPluginSetWritesAGatedSettingsPatch(t *testing.T) {
	guardPluginServer(t, "")
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(storedGuardPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "action.json")
	if _, err := captureStdout(t, func() error {
		return runGuardPluginSet(guardPluginCmdWith(map[string]string{"file": policy, "out": out}), nil)
	}); err != nil {
		t.Fatal(err)
	}
	action := readAction(t, out) // also passes the decision-request envelope check
	if action.Type != "workspace.settings.patch" {
		t.Fatalf("type %q", action.Type)
	}
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(action.Params["settings"], &settings)
	if len(settings) != 1 || !json.Valid(settings["guard_policy"]) {
		t.Fatalf("settings = %s", action.Params["settings"])
	}
	// The key is on the CHE-5167 allow table, so the gate accepts the patch.
	if err := decisionaction.CheckConfigChange(decisionaction.SettingsField, []string{"guard_policy"}); err != nil {
		t.Fatal(err)
	}
	want, _ := decisionaction.SettingsDigest("ws-1", map[string]json.RawMessage{"guard_policy": settings["guard_policy"]})
	if action.str(t, "after_sha256") != want {
		t.Fatal("after digest does not cover the compact policy")
	}
}

func TestGuardPluginSetRefusesBadPolicyBeforeReading(t *testing.T) {
	seen := proposeServer(t, map[string]any{})
	dir := t.TempDir()
	for name, body := range map[string]string{
		"wrong version": `{"version":3}`,
		"script field":  `{"version":1,"plugins":[],"script":"x"}`,
		"not json":      `{`,
	} {
		t.Run(name, func(t *testing.T) {
			f := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
			_ = os.WriteFile(f, []byte(body), 0o600)
			out := filepath.Join(dir, "action-"+strings.ReplaceAll(name, " ", "_"))
			_, err := captureStdout(t, func() error {
				return runGuardPluginSet(guardPluginCmdWith(map[string]string{"file": f, "out": out}), nil)
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if _, statErr := os.Stat(out); statErr == nil {
				t.Fatal("action file written for a bad policy")
			}
		})
	}
	if len(*seen) != 0 {
		t.Fatalf("a refused policy read the server: %v", *seen)
	}
}
