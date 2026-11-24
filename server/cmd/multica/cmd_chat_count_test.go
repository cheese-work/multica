package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestChatCountReadsCountOnly(test *testing.T) {
	command, _, err := chatCmd.Find([]string{"count"})
	if err != nil || command == chatCmd {
		test.Fatal("chat count command is missing")
	}
	var got string
	useChatTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
		got = request.Method + " " + request.URL.Path + "?" + request.URL.RawQuery
		_ = json.NewEncoder(writer).Encode(map[string]any{"count": 3, "since": "2026-11-23T00:00:00Z", "until": "2026-11-24T00:00:00Z"})
	})
	_ = command.Flags().Set("member", "11111111-1111-1111-1111-111111111111")
	_ = command.Flags().Set("kind", "progress-card")
	_ = command.Flags().Set("since", "2026-11-23T00:00:00Z")
	_ = command.Flags().Set("output", "json")
	out, err := captureStdout(test, func() error { return command.RunE(command, nil) })
	if err != nil {
		test.Fatal(err)
	}
	for _, want := range []string{"GET /api/chat/count?", "kind=progress-card", "member=11111111-1111-1111-1111-111111111111", "since=2026-11-23T00%3A00%3A00Z"} {
		if !strings.Contains(got, want) {
			test.Errorf("request %q missing %q", got, want)
		}
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil || resp["count"] != float64(3) {
		test.Fatalf("wrong output: %s error=%v", out, err)
	}
}

func TestChatCountRequiresMemberAndSince(test *testing.T) {
	command, _, _ := chatCmd.Find([]string{"count"})
	if command == nil || command == chatCmd {
		test.Fatal("chat count command is missing")
	}
	_ = command.Flags().Set("member", "")
	_ = command.Flags().Set("since", "")
	if err := command.RunE(command, nil); err == nil || !strings.Contains(err.Error(), "--member") {
		test.Fatalf("want --member error, got %v", err)
	}
	_ = command.Flags().Set("member", "m")
	if err := command.RunE(command, nil); err == nil || !strings.Contains(err.Error(), "--since") {
		test.Fatalf("want --since error, got %v", err)
	}
}
