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

func repoCreateCmdWith(flags map[string]string) *cobra.Command {
	c := &cobra.Command{Use: "repo-create"}
	for _, name := range []string{"org", "name", "template", "description", "visibility", "out"} {
		c.Flags().String(name, "", "")
	}
	c.Flags().Bool("private", false, "")
	c.Flags().Bool("dry-run", false, "")
	c.Flags().Int("expires-in", 0, "")
	for k, v := range flags {
		_ = c.Flags().Set(k, v)
	}
	return c
}

func repoCreateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MULTICA_WORKSPACE_ID", "00000000-0000-4000-8000-000000000001")
}

func TestGitHubRepoCreateWritesAnActionFileTheServerAccepts(t *testing.T) {
	repoCreateEnv(t)
	out := filepath.Join(t.TempDir(), "action.json")
	stdout, err := captureStdout(t, func() error {
		return runGitHubRepoCreate(repoCreateCmdWith(map[string]string{
			"org": "tevo-studio", "name": "we006", "template": "tevo-studio/code-android-template",
			"private": "true", "description": "WE-006 app", "out": out, "expires-in": "3600",
		}), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	action := readAction(t, out) // also passes the decision-request envelope check
	if action.Type != decisionaction.GitHubRepoCreate || action.Version != 1 || action.Expires != 3600 {
		t.Fatalf("action = %+v", action)
	}
	// The same normalizer the server runs accepts the params.
	if _, err := decisionaction.Normalize(decisionaction.Request{Type: action.Type, Version: 1, Params: mustRaw(t, action.Params)}); err != nil {
		t.Fatalf("server normalizer refused the file: %v", err)
	}
	for key, want := range map[string]string{"workspace_id": `"00000000-0000-4000-8000-000000000001"`, "org": `"tevo-studio"`, "name": `"we006"`, "visibility": `"private"`, "template": `"tevo-studio/code-android-template"`} {
		if string(action.Params[key]) != want {
			t.Fatalf("params[%s] = %s, want %s", key, action.Params[key], want)
		}
	}
	if !strings.Contains(stdout, "decision request <issue> --action-file "+out) {
		t.Fatalf("stdout lacks the next step: %q", stdout)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("action file mode = %v (%v), want 0600", info, err)
	}
}

func TestGitHubRepoCreateDefaultsToPrivate(t *testing.T) {
	repoCreateEnv(t)
	out := filepath.Join(t.TempDir(), "action.json")
	if _, err := captureStdout(t, func() error {
		return runGitHubRepoCreate(repoCreateCmdWith(map[string]string{"org": "cheese-work", "name": "x", "out": out}), nil)
	}); err != nil {
		t.Fatal(err)
	}
	if got := string(readAction(t, out).Params["visibility"]); got != `"private"` {
		t.Fatalf("visibility = %s, want private", got)
	}
}

func TestGitHubRepoCreateDryRunPrintsAndWritesNothing(t *testing.T) {
	repoCreateEnv(t)
	dir := t.TempDir()
	stdout, err := captureStdout(t, func() error {
		return runGitHubRepoCreate(repoCreateCmdWith(map[string]string{"org": "tevo-studio", "name": "we006", "dry-run": "true", "out": filepath.Join(dir, "never.json")}), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"type": "github.repo.create"`) {
		t.Fatalf("dry run did not print the action: %q", stdout)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("dry run wrote %d files", len(entries))
	}
}

func TestGitHubRepoCreateNeedsOutOrDryRun(t *testing.T) {
	repoCreateEnv(t)
	_, err := captureStdout(t, func() error {
		return runGitHubRepoCreate(repoCreateCmdWith(map[string]string{"org": "tevo-studio", "name": "we006"}), nil)
	})
	if err == nil || !strings.Contains(err.Error(), "--out") {
		t.Fatalf("err = %v, want an --out error", err)
	}
}

func TestGitHubRepoCreateRefusesWhatTheServerRefusesBeforeWritingAnything(t *testing.T) {
	repoCreateEnv(t)
	for name, flags := range map[string]map[string]string{
		"org not approved":      {"org": "evil-org", "name": "x"},
		"template other org":    {"org": "tevo-studio", "name": "x", "template": "evil-org/t"},
		"bad name":              {"org": "tevo-studio", "name": "../x"},
		"missing org":           {"name": "x"},
		"missing name":          {"org": "tevo-studio"},
		"unknown visibility":    {"org": "tevo-studio", "name": "x", "visibility": "secret"},
		"private and public":    {"org": "tevo-studio", "name": "x", "private": "true", "visibility": "public"},
		"description too large": {"org": "tevo-studio", "name": "x", "description": strings.Repeat("a", 400)},
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "action.json")
			flags["out"] = out
			if _, err := captureStdout(t, func() error { return runGitHubRepoCreate(repoCreateCmdWith(flags), nil) }); err == nil {
				t.Fatal("expected an error")
			}
			if _, err := os.Stat(out); err == nil {
				t.Fatal("action file written for a refused request")
			}
		})
	}
}

func TestGitHubRepoCreatePublicNamesTheClassItNeeds(t *testing.T) {
	repoCreateEnv(t)
	out := filepath.Join(t.TempDir(), "action.json")
	stdout, err := captureStdout(t, func() error {
		return runGitHubRepoCreate(repoCreateCmdWith(map[string]string{"org": "tevo-studio", "name": "x", "visibility": "public", "out": out}), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "--class publish") {
		t.Fatalf("public output lacks the publish hint: %q", stdout)
	}
}

func TestGitHubRepoCreateIsWiredUnderGitHub(t *testing.T) {
	var found *cobra.Command
	for _, c := range githubCmd.Commands() {
		if c.Name() == "repo-create" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("github has no repo-create command")
	}
	for _, flag := range []string{"org", "name", "template", "private", "visibility", "description", "out", "dry-run", "expires-in"} {
		if found.Flags().Lookup(flag) == nil {
			t.Errorf("repo-create has no --%s flag", flag)
		}
	}
}

func mustRaw(t *testing.T, params map[string]json.RawMessage) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
