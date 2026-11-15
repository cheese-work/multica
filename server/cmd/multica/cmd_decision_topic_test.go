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

// waiverAskFile is a waiver ask on disk with the given ask.topic (empty: none).
func waiverAskFile(test *testing.T, topic string) string {
	test.Helper()
	ask := structuredAskFixture()
	for index, key := range []string{"waive", "refuse"} {
		ask["options"].([]map[string]any)[index]["option_key"] = key
	}
	ask["recommended_option"] = "Tomorrow"
	ask["waiver"] = map[string]any{"repo": "cheese-work/multica", "head_sha": strings.Repeat("a", 40), "red_impossible_category": "other", "weaker_evidence_kind": "review_only", "cross_lab_pass_on_head": true, "tests_retrospective": false, "change_kind": "docs"}
	if topic != "" {
		ask["topic"] = topic
	}
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

func runAskWithTopic(test *testing.T, topic, askTopic string) (map[string]any, error) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewDecoder(request.Body).Decode(&body)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	command := newDecisionAskCmd()
	for name, value := range map[string]string{"ask-file": waiverAskFile(test, askTopic), "class": "reversible", "topic": topic} {
		if value == "" {
			continue
		}
		if err := command.Flags().Set(name, value); err != nil {
			return nil, err
		}
	}
	_, err := captureStdout(test, func() error { return command.RunE(command, nil) })
	return body, err
}

func TestDecisionAskTopicFlagNamesTheWaiverTopic(test *testing.T) {
	body, err := runAskWithTopic(test, "test_first_waiver", "test_first_waiver")
	if err != nil {
		test.Fatal(err)
	}
	if ask, _ := body["ask"].(map[string]any); ask["topic"] != "test_first_waiver" || ask["waiver"] == nil {
		test.Fatalf("posted ask = %v, want topic test_first_waiver and the waiver block", body["ask"])
	}
}

func TestDecisionAskTopicFlagIsClosedAndAgreesWithTheFile(test *testing.T) {
	if _, err := runAskWithTopic(test, "free_lunch", "test_first_waiver"); err == nil || !strings.Contains(err.Error(), "test_first_waiver") {
		test.Fatalf("unknown topic: err = %v, want a refusal naming test_first_waiver", err)
	}
	// An ask file with no topic (and so no waiver block, which DecodeAsk refuses) cannot carry the flag's topic.
	if _, err := runAskWithTopic(test, "test_first_waiver", "other_topic"); err == nil {
		test.Fatal("--topic that disagrees with ask.topic must be refused")
	}
}
