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

func TestDecisionShadowReportSendsWindow(test *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotQuery = request.URL.RawQuery
		_ = json.NewEncoder(writer).Encode(map[string]any{"route": map[string]any{"eligible_exits": 3}})
	}))
	defer server.Close()
	test.Chdir(test.TempDir())
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mul_fake-member-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")

	command, _, err := rootCmd.Find([]string{"decision", "shadow-report"})
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = command.Flags().Set("since", ""); _ = command.Flags().Set("until", "") })
	if err := command.Flags().Set("since", "2026-01-10T00:00:00+07:00"); err != nil {
		test.Fatalf("--since flag: %v", err)
	}
	if err := command.Flags().Set("until", "2026-01-11T00:00:00+07:00"); err != nil {
		test.Fatalf("--until flag: %v", err)
	}
	if _, err := captureStdout(test, func() error { return command.RunE(command, nil) }); err != nil {
		test.Fatal(err)
	}
	if want := "since=2026-01-10T00%3A00%3A00%2B07%3A00&until=2026-01-11T00%3A00%3A00%2B07%3A00"; gotQuery != want {
		test.Fatalf("query = %q, want %q", gotQuery, want)
	}

	_ = command.Flags().Set("since", "")
	_ = command.Flags().Set("until", "")
	if _, err := captureStdout(test, func() error { return command.RunE(command, nil) }); err != nil {
		test.Fatal(err)
	}
	if gotQuery != "" {
		test.Fatalf("no flags sent query %q", gotQuery)
	}
}
