package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecisionAskPostsOnlyTheAskToTheOneStepRoute(test *testing.T) {
	for _, words := range [][]string{{"decision", "ask"}, {"agent", "decision", "ask"}} {
		test.Run(strings.Join(words, " "), func(test *testing.T) { postDecisionAsk(test, words) })
	}
}

func postDecisionAsk(test *testing.T, words []string) {
	var path string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		_ = json.NewDecoder(request.Body).Decode(&body)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	command, _, err := rootCmd.Find(words)
	if err != nil || command.Name() != "ask" {
		test.Fatalf("%v is unavailable: %v", words, err)
	}
	askPath := filepath.Join(test.TempDir(), "ask.json")
	if err := os.WriteFile(askPath, []byte(structuredAskJSON(test, "Ship it?")), 0o600); err != nil {
		test.Fatal(err)
	}
	for name, value := range map[string]string{"ask-file": askPath, "class": "reversible"} {
		if err := command.Flags().Set(name, value); err != nil {
			test.Fatal(err)
		}
	}
	if _, err := captureStdout(test, func() error { return command.RunE(command, nil) }); err != nil {
		test.Fatal(err)
	}
	if path != "/api/decisions/ask" || body["escalation_class"] != "reversible" || body["issue_id"] != nil || body["for"] != nil || body["ask"] == nil {
		test.Fatalf("POST %s body %v, want only ask, footer, parent_id and escalation_class", path, body)
	}
}

// The --ask-file help names options[].design and says frame_updated_at is the server's (CHE-7064 S10).
func TestAskFileHelpNamesTheDesignOptionField(test *testing.T) {
	help := decisionRequestCmd.Flags().Lookup("ask-file").Usage
	for _, want := range []string{"design {frame_url, attestation_id}", "frame_updated_at", "design_version_server_owned"} {
		if !strings.Contains(help, want) {
			test.Errorf("--ask-file help does not mention %q", want)
		}
	}
}

// designAskFile writes an ask whose first option cites a doop frame, the way an
// agent writes it: the version is the server's, so the file holds none.
func designAskFile(test *testing.T) string {
	test.Helper()
	var ask map[string]any
	if err := json.Unmarshal([]byte(structuredAskJSON(test, "Pick the layout?")), &ask); err != nil {
		test.Fatal(err)
	}
	ask["options"].([]any)[0].(map[string]any)["design"] = map[string]any{"frame_url": "https://doop.example.com/c/cv1?frame=f1", "attestation_id": "run-1"}
	content, err := json.Marshal(ask)
	if err != nil {
		test.Fatal(err)
	}
	path := filepath.Join(test.TempDir(), "ask.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		test.Fatal(err)
	}
	return path
}

func runDecisionAskFile(test *testing.T, serverURL, askPath string) (string, error) {
	test.Helper()
	test.Setenv("MULTICA_SERVER_URL", serverURL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	command, _, err := rootCmd.Find([]string{"decision", "ask"})
	if err != nil {
		test.Fatal(err)
	}
	for name, value := range map[string]string{"ask-file": askPath, "class": "reversible"} {
		if err := command.Flags().Set(name, value); err != nil {
			test.Fatal(err)
		}
	}
	return captureStdout(test, func() error { return command.RunE(command, nil) })
}

// G4 (CHE-7064 S12): the CLI sends options[].design as written, with no version.
func TestDecisionAskSendsTheDesignOptionAsWritten(test *testing.T) {
	var body struct {
		Ask struct {
			Options []struct {
				Design map[string]any `json:"design"`
			} `json:"options"`
		} `json:"ask"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewDecoder(request.Body).Decode(&body)
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
	}))
	defer server.Close()
	if _, err := runDecisionAskFile(test, server.URL, designAskFile(test)); err != nil {
		test.Fatal(err)
	}
	design := body.Ask.Options[0].Design
	if design["frame_url"] != "https://doop.example.com/c/cv1?frame=f1" || design["attestation_id"] != "run-1" || design["frame_updated_at"] != nil {
		test.Fatalf("design = %v, want the frame and the attestation only", design)
	}
}

// A refusal of the design guard reaches the person at the terminal with its status and code, and the command fails.
func TestDecisionAskShowsTheDesignGuardRefusalCode(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"code":"design_verdict_missing","error":"the newest design verdict for this attestation is not PASS"}`))
	}))
	defer server.Close()
	_, err := runDecisionAskFile(test, server.URL, designAskFile(test))
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "design_verdict_missing") {
		test.Fatalf("error = %v, want the 409 status and the design_verdict_missing code", err)
	}
}
