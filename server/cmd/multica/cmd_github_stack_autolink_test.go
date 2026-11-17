package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// CHE-4165 (CHE-4143 step 5): `multica github stack-autolink status|set`.

func newStackAutolinkTestCmd(output, repo, mode string) *cobra.Command {
	cmd := &cobra.Command{Use: "stack-autolink"}
	cmd.Flags().String("repo", repo, "")
	cmd.Flags().String("mode", mode, "")
	cmd.Flags().String("output", output, "")
	return cmd
}

func TestGitHubStackAutolinkStatusReadsFinalAndPerWorkspaceModes(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("status sent %s, want GET (read-only)", r.Method)
		}
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "acme/app", "final_mode": "report", "env_kill": false, "hidden_workspaces": 1,
			"workspaces": []map[string]any{{"workspace_id": "w-1", "name": "Alpha", "effective_mode": "link"}},
		})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	out, err := captureStdout(t, func() error {
		return runGitHubStackAutolinkStatus(newStackAutolinkTestCmd("table", "acme/app", ""), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/github/stack-autolink" || gotQuery != "repo=acme%2Fapp" {
		t.Fatalf("request = %s?%s, want /api/github/stack-autolink?repo=acme%%2Fapp", gotPath, gotQuery)
	}
	for _, want := range []string{"report", "Alpha", "link", "1 other workspace"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table output missing %q:\n%s", want, out)
		}
	}

	js, err := captureStdout(t, func() error {
		return runGitHubStackAutolinkStatus(newStackAutolinkTestCmd("json", "acme/app", ""), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		FinalMode  string `json:"final_mode"`
		Workspaces []struct {
			EffectiveMode string `json:"effective_mode"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(js), &parsed); err != nil || parsed.FinalMode != "report" || len(parsed.Workspaces) != 1 || parsed.Workspaces[0].EffectiveMode != "link" {
		t.Fatalf("--output json = %q, %v", js, err)
	}
}

func TestGitHubStackAutolinkStatusRequiresRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("status without --repo must not call the server: %s %s", r.Method, r.URL)
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	err := runGitHubStackAutolinkStatus(newStackAutolinkTestCmd("table", "", ""), nil)
	if err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Fatalf("err = %v, want a --repo error", err)
	}
}

func TestGitHubStackAutolinkSetSendsModeAndOptionalRepo(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/github/stack-autolink" {
			t.Fatalf("set sent %s %s, want PUT /api/github/stack-autolink", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"workspace_id": "w-1", "repo": body["repo"], "mode": body["mode"]})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	if _, err := captureStdout(t, func() error {
		return runGitHubStackAutolinkSet(newStackAutolinkTestCmd("table", "acme/app", "link"), nil)
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return runGitHubStackAutolinkSet(newStackAutolinkTestCmd("json", "", "off"), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0]["repo"] != "acme/app" || bodies[0]["mode"] != "link" {
		t.Fatalf("bodies = %v, want repo+mode then mode only", bodies)
	}
	if _, has := bodies[1]["repo"]; has || bodies[1]["mode"] != "off" {
		t.Fatalf("workspace-wide set body = %v, want mode off and no repo", bodies[1])
	}
	if !strings.Contains(out, `"mode": "off"`) {
		t.Fatalf("--output json = %q, want the response", out)
	}
}

func TestGitHubStackAutolinkSetRejectsBadModeBeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("an invalid mode must not reach the server: %s %s", r.Method, r.URL)
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	for _, mode := range []string{"", "strict", "LINK "} {
		err := runGitHubStackAutolinkSet(newStackAutolinkTestCmd("table", "", mode), nil)
		if err == nil || !strings.Contains(err.Error(), "off, report or link") {
			t.Fatalf("mode %q: err = %v, want an off/report/link error", mode, err)
		}
	}
}

func TestGitHubStackAutolinkHelpAndWiring(t *testing.T) {
	var found *cobra.Command
	for _, c := range githubCmd.Commands() {
		if c.Name() == "stack-autolink" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("github has no stack-autolink command")
	}
	subs := map[string]*cobra.Command{}
	for _, c := range found.Commands() {
		subs[c.Name()] = c
	}
	status, set := subs["status"], subs["set"]
	if status == nil || set == nil {
		t.Fatalf("stack-autolink subcommands = %v, want status and set", subs)
	}
	for _, flag := range []string{"repo", "output"} {
		if status.Flags().Lookup(flag) == nil {
			t.Errorf("status has no --%s", flag)
		}
	}
	for _, flag := range []string{"repo", "mode", "output"} {
		if set.Flags().Lookup(flag) == nil {
			t.Errorf("set has no --%s", flag)
		}
	}
	help := found.Long + set.Long + status.Long
	for _, want := range []string{"off", "report", "link", "owner", "admin"} {
		if !strings.Contains(help, want) {
			t.Errorf("help text never mentions %q", want)
		}
	}
	if status.Args == nil || set.Args == nil {
		t.Error("status and set must reject stray arguments")
	}
}
