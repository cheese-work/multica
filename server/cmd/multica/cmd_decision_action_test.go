package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const actionFileJSON = `{"type":"agent.env.patch","version":1,"params":{"agent_id":"a","set":{"K":"v"}},"secret_slots":[{"slot":"S","target":{"agent_id":"a","env_key":"S"}}],"expires_in_seconds":3600}`

// runActionRequest posts a request with the given --action-file text and
// returns the posted body and the number of POSTs to /api/decisions.
func runActionRequest(test *testing.T, actionText string) (map[string]any, int, error) {
	test.Helper()
	command := decisionRequestCmd
	for _, name := range []string{"issue", "session", "for", "new-dm", "class", "ask-file", "action-file"} {
		flag := command.Flags().Lookup(name)
		if flag == nil {
			test.Fatalf("missing flag %s", name)
		}
		original := flag.Value.String()
		test.Cleanup(func() { _ = command.Flags().Set(name, original) })
	}
	dir := test.TempDir()
	askPath := filepath.Join(dir, "ask.json")
	if err := os.WriteFile(askPath, []byte(structuredAskJSON(test, "Apply env")), 0600); err != nil {
		test.Fatal(err)
	}
	actionPath := ""
	if actionText != "" {
		actionPath = filepath.Join(dir, "action.json")
		if err := os.WriteFile(actionPath, []byte(actionText), 0600); err != nil {
			test.Fatal(err)
		}
	}
	var body map[string]any
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/decisions" {
			posts++
			_ = json.NewDecoder(request.Body).Decode(&body)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	for name, value := range map[string]string{"issue": "11111111-1111-4111-8111-111111111111", "session": "", "new-dm": "false", "for": "human-id", "class": "reversible", "ask-file": askPath, "action-file": actionPath} {
		_ = command.Flags().Set(name, value)
	}
	_, err := captureStdout(test, func() error { return runDecisionRequest(command, nil) })
	return body, posts, err
}

func TestDecisionRequestSendsTheActionFile(test *testing.T) {
	body, posts, err := runActionRequest(test, actionFileJSON)
	if err != nil || posts != 1 {
		test.Fatalf("posts=%d error=%v", posts, err)
	}
	var want, got any
	_ = json.Unmarshal([]byte(actionFileJSON), &want)
	got = body["action"]
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		test.Fatalf("action = %s, want %s", gotJSON, wantJSON)
	}
}

func TestDecisionRequestWithoutActionFileSendsNoAction(test *testing.T) {
	body, posts, err := runActionRequest(test, "")
	if err != nil || posts != 1 {
		test.Fatalf("posts=%d error=%v", posts, err)
	}
	if _, present := body["action"]; present {
		test.Fatalf("action key sent without --action-file: %+v", body["action"])
	}
}

func TestDecisionRequestRefusesABadActionFile(test *testing.T) {
	for name, text := range map[string]string{
		"not json":       `{"type":`,
		"not an object":  `["agent.env.patch"]`,
		"no type":        `{"version":1,"params":{}}`,
		"no version":     `{"type":"agent.env.patch","params":{}}`,
		"unknown key":    `{"type":"agent.env.patch","version":1,"params":{},"secrets":{"K":"v"}}`,
		"trailing value": actionFileJSON + ` {}`,
		"blank":          "  \n",
	} {
		test.Run(name, func(test *testing.T) {
			if _, posts, err := runActionRequest(test, text); err == nil || posts != 0 {
				test.Fatalf("accepted a bad action file: posts=%d error=%v", posts, err)
			}
		})
	}
}
