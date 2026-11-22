package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// The CLI sends wait: true only when --wait is set, on both commands (CHE-6460 D1b).
func TestDecisionWaitFlagIsSentOnlyWhenSet(test *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	cases := []struct {
		name  string
		words []string
		path  string
		setup func(*testing.T, *cobra.Command)
	}{
		{"request", []string{"decision", "request"}, "/api/decisions", func(test *testing.T, command *cobra.Command) {
			setDecisionFlags(test, command, map[string]string{"issue": issueID, "session": "33333333-3333-4333-8333-333333333333", "class": "reversible"})
		}},
		{"ask", []string{"decision", "ask"}, "/api/decisions/ask", func(test *testing.T, command *cobra.Command) {
			setDecisionFlags(test, command, map[string]string{"class": "reversible"})
		}},
	}
	for _, tc := range cases {
		for _, waitSet := range []bool{false, true} {
			name := tc.name + "/without --wait"
			if waitSet {
				name = tc.name + "/with --wait"
			}
			test.Run(name, func(test *testing.T) {
				var body map[string]any
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodGet {
						_ = json.NewEncoder(writer).Encode(map[string]any{"id": issueID, "identifier": "CHE-1"})
						return
					}
					_ = json.NewDecoder(request.Body).Decode(&body)
					_ = json.NewEncoder(writer).Encode(map[string]any{"id": "44444444-4444-4444-8444-444444444444"})
				}))
				defer server.Close()
				test.Setenv("MULTICA_SERVER_URL", server.URL)
				test.Setenv("MULTICA_TOKEN", "mat_test-token")
				test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
				command, _, err := rootCmd.Find(tc.words)
				if err != nil || command.Name() != tc.words[len(tc.words)-1] {
					test.Fatalf("%v is unavailable: %v", tc.words, err)
				}
				tc.setup(test, command)
				if waitSet {
					flag := command.Flags().Lookup("wait")
					if flag == nil {
						test.Fatalf("%v has no --wait flag", tc.words)
					}
					if err := command.Flags().Set("wait", "true"); err != nil {
						test.Fatal(err)
					}
					test.Cleanup(func() { _ = command.Flags().Set("wait", "false") })
				}
				if _, err := captureStdout(test, func() error { return command.RunE(command, nil) }); err != nil {
					test.Fatal(err)
				}
				got, present := body["wait"]
				if waitSet && got != true {
					test.Fatalf("with --wait, body wait = %v, want true: %+v", got, body)
				}
				if !waitSet && present {
					test.Fatalf("without --wait, body carries wait = %v: %+v", got, body)
				}
			})
		}
	}
}

func setDecisionFlags(test *testing.T, command *cobra.Command, flags map[string]string) {
	test.Helper()
	path := filepath.Join(test.TempDir(), "ask.json")
	if err := os.WriteFile(path, []byte(structuredAskJSON(test, "Ship it?")), 0o600); err != nil {
		test.Fatal(err)
	}
	flags["ask-file"] = path
	for name, value := range flags {
		if err := command.Flags().Set(name, value); err != nil {
			test.Fatal(err)
		}
	}
}
