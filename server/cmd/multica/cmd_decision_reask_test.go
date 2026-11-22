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

// CHE-6433: --reask-of and --new-topic reach the server as reask_of and new_topic,
// exclude each other, and a plain request sends neither.
func TestDecisionRequestReaskFlags(test *testing.T) {
	command := decisionRequestCmd
	names := []string{"issue", "session", "for", "new-dm", "parent", "class", "ask-file", "reask-of", "new-topic"}
	for _, name := range names {
		flag := command.Flags().Lookup(name)
		if flag == nil {
			test.Fatalf("missing flag %s", name)
		}
		original := flag.Value.String()
		test.Cleanup(func() { _ = command.Flags().Set(name, original) })
	}
	askPath := filepath.Join(test.TempDir(), "ask.txt")
	if err := os.WriteFile(askPath, []byte(structuredAskJSON(test, "Pick again")), 0600); err != nil {
		test.Fatal(err)
	}
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/decisions" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			test.Error(err)
		}
		bodies = append(bodies, body)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "request-id"})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	reset := func(values map[string]string) {
		for _, name := range names {
			_ = command.Flags().Set(name, "")
		}
		_ = command.Flags().Set("new-dm", "false")
		_ = command.Flags().Set("new-topic", "false")
		base := map[string]string{"issue": "11111111-1111-4111-8111-111111111111", "for": "human-id", "class": "reversible", "ask-file": askPath}
		for name, value := range values {
			base[name] = value
		}
		for name, value := range base {
			_ = command.Flags().Set(name, value)
		}
	}
	const earlier = "22222222-2222-4222-8222-222222222222"

	reset(map[string]string{"reask-of": earlier})
	if _, err := captureStdout(test, func() error { return runDecisionRequest(command, nil) }); err != nil || len(bodies) != 1 {
		test.Fatalf("--reask-of: bodies=%d error=%v", len(bodies), err)
	}
	if bodies[0]["reask_of"] != earlier || bodies[0]["new_topic"] != nil {
		test.Errorf("--reask-of body: %+v", bodies[0])
	}

	reset(map[string]string{"new-topic": "true"})
	if _, err := captureStdout(test, func() error { return runDecisionRequest(command, nil) }); err != nil || len(bodies) != 2 {
		test.Fatalf("--new-topic: bodies=%d error=%v", len(bodies), err)
	}
	if bodies[1]["new_topic"] != true || bodies[1]["reask_of"] != nil {
		test.Errorf("--new-topic body: %+v", bodies[1])
	}

	reset(nil)
	if _, err := captureStdout(test, func() error { return runDecisionRequest(command, nil) }); err != nil || len(bodies) != 3 {
		test.Fatalf("plain: bodies=%d error=%v", len(bodies), err)
	}
	if bodies[2]["reask_of"] != nil || bodies[2]["new_topic"] != nil {
		test.Errorf("a plain request sends neither flag: %+v", bodies[2])
	}

	for name, values := range map[string]map[string]string{
		"both":        {"reask-of": earlier, "new-topic": "true"},
		"with parent": {"reask-of": earlier, "parent": earlier},
		"with new-dm": {"reask-of": earlier, "new-dm": "true"},
		"without for": {"reask-of": earlier, "for": "", "session": "lane-id"},
	} {
		reset(values)
		if err := runDecisionRequest(command, nil); err == nil || !strings.Contains(err.Error(), "--reask-of") {
			test.Errorf("%s: error = %v, want a --reask-of refusal", name, err)
		}
	}
	if len(bodies) != 3 {
		test.Errorf("a refused flag mix reached the server: %d bodies", len(bodies))
	}
}
