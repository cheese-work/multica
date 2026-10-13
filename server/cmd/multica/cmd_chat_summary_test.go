package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRunChatSummarySet(test *testing.T) {
	for _, scenario := range []struct {
		name, content string
		valid         bool
	}{
		{"Unicode boundary", strings.Repeat("🙂", 600), true},
		{"clear", "", true},
		{"too long", strings.Repeat("🙂", 601), false},
		{"invalid UTF-8", string([]byte{0xff}), false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				if request.Method != http.MethodPut || request.URL.Path != "/api/chat/summary" || request.Header.Get("Authorization") != "Bearer mat_test" {
					test.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
				}
				var body map[string]string
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["lane_summary"] != scenario.content {
					test.Errorf("summary changed: %v, %v", body, err)
				}
				json.NewEncoder(writer).Encode(map[string]any{"lane_summary": scenario.content})
			}))
			defer server.Close()
			test.Setenv("MULTICA_SERVER_URL", server.URL)
			test.Setenv("MULTICA_WORKSPACE_ID", "ws-test")
			test.Setenv("MULTICA_TOKEN", "mat_test")
			path := filepath.Join(test.TempDir(), "summary.txt")
			if err := os.WriteFile(path, []byte(scenario.content), 0600); err != nil {
				test.Fatal(err)
			}
			command := &cobra.Command{}
			command.SetOut(io.Discard)
			command.Flags().String("file", path, "")
			err := runChatSummarySet(command, nil)
			if scenario.valid && (err != nil || calls != 1) {
				test.Fatalf("valid summary: err=%v, calls=%d", err, calls)
			}
			if !scenario.valid && (err == nil || calls != 0) {
				test.Fatalf("invalid summary sent: err=%v, calls=%d", err, calls)
			}
		})
	}
}

func TestChatSummarySetCommand(test *testing.T) {
	command, _, err := chatCmd.Find([]string{"summary", "set"})
	if err != nil || command.Use != "set" || command.Flags().Lookup("file") == nil {
		test.Fatalf("summary command not registered: %v, %v", command, err)
	}
	if err := command.ValidateRequiredFlags(); err == nil {
		test.Fatal("--file is not required")
	}
}
