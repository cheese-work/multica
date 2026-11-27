package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// CHE-10268 (S3 of CHE-10198): `multica github pr-signers`.

const prSignersTestBody = `{"repo":"acme/widget","pr":206,"head_sha":"0123456789012345678901234567890123456789","issue_id":"01a216e3-7195-7649-819f-023aafeeb7f4","linked":true,"comment_protection":true,"pr_author":{"agent_id":"11111111-1111-4111-8111-111111111111","agent_name":"author-agent","lab":"anthropic"},"signers":[{"agent_id":"22222222-2222-4222-8222-222222222222","agent_name":"reviewer-agent","lab":"openai","comment_id":"33333333-3333-4333-8333-333333333333","verdict_at":"2026-11-27T03:00:00Z"}],"future_field":"kept"}`

func newPRSignersTestCmd(output, repo string, pr int, head, issue string) *cobra.Command {
	cmd := &cobra.Command{Use: "pr-signers"}
	cmd.Flags().String("repo", repo, "")
	cmd.Flags().Int("pr", pr, "")
	cmd.Flags().String("head", head, "")
	cmd.Flags().String("issue", issue, "")
	cmd.Flags().String("output", output, "")
	return cmd
}

func TestGitHubPRSignersSendsTheFlagsAsAGetAndPrintsTheContractJSON(t *testing.T) {
	var gotMethod, gotPath string
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.Query()
		_, _ = w.Write([]byte(prSignersTestBody))
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	out, err := captureStdout(t, func() error {
		return runGitHubPRSigners(newPRSignersTestCmd("json", "acme/widget", 206, "0123456789012345678901234567890123456789", "CHE-10198"), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/github/pull-requests/signers" {
		t.Fatalf("request = %s %s, want GET /api/github/pull-requests/signers", gotMethod, gotPath)
	}
	for key, want := range map[string]string{"repo": "acme/widget", "pr": "206", "head": "0123456789012345678901234567890123456789", "issue": "CHE-10198"} {
		if got := gotQuery[key]; len(got) != 1 || got[0] != want {
			t.Errorf("query %s = %v, want %q", key, got, want)
		}
	}
	var parsed struct {
		Signers []struct {
			AgentName string `json:"agent_name"`
		} `json:"signers"`
		FutureField       string `json:"future_field"`
		CommentProtection bool   `json:"comment_protection"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || len(parsed.Signers) != 1 || parsed.Signers[0].AgentName != "reviewer-agent" || !parsed.CommentProtection || parsed.FutureField != "kept" {
		t.Fatalf("--output json = %q, %v: want the server body unchanged", out, err)
	}
}

func TestGitHubPRSignersTableListsTheAuthorAndSigners(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(prSignersTestBody))
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	out, err := captureStdout(t, func() error {
		return runGitHubPRSigners(newPRSignersTestCmd("table", "acme/widget", 206, "0123456789012345678901234567890123456789", "CHE-10198"), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"author-agent", "reviewer-agent", "openai", "comment protection: on"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestGitHubPRSignersRequiresEveryFlagBeforeCallingTheServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("an incomplete command must not call the server: %s %s", r.Method, r.URL)
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	const head = "0123456789012345678901234567890123456789"
	for flag, cmd := range map[string]*cobra.Command{
		"--repo":  newPRSignersTestCmd("json", "", 206, head, "CHE-1"),
		"--pr":    newPRSignersTestCmd("json", "a/b", 0, head, "CHE-1"),
		"--head":  newPRSignersTestCmd("json", "a/b", 206, "", "CHE-1"),
		"--issue": newPRSignersTestCmd("json", "a/b", 206, head, ""),
	} {
		if err := runGitHubPRSigners(cmd, nil); err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("missing %s: err = %v, want an error naming the flag", flag, err)
		}
	}
}

func TestGitHubPRSignersSurfacesAServerRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"issue is not linked to the pull request"}`))
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	err := runGitHubPRSigners(newPRSignersTestCmd("json", "a/b", 206, "0123456789012345678901234567890123456789", "CHE-1"), nil)
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("err = %v, want the 409 from the server", err)
	}
}
