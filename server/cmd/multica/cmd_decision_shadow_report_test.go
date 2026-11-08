package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecisionShadowReport(test *testing.T) {
	var gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath, gotMethod = request.URL.Path, request.Method
		_ = json.NewEncoder(writer).Encode(map[string]any{"decision": map[string]any{"decided": 7, "ready": false, "classes": []any{}}})
	}))
	defer server.Close()
	test.Chdir(test.TempDir())
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mul_fake-member-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")

	command, _, err := rootCmd.Find([]string{"decision", "shadow-report"})
	if err != nil || command.Name() != "shadow-report" {
		test.Fatalf("decision shadow-report not found: %v", err)
	}
	out, err := captureStdout(test, func() error { return command.RunE(command, nil) })
	if err != nil {
		test.Fatal(err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/decisions/shadow-report" {
		test.Fatalf("called %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(out, `"decided": 7`) {
		test.Fatalf("endpoint JSON not printed: %s", out)
	}
}
