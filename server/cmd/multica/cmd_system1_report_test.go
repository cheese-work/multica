package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSystem1ReportSendsScope(test *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath, gotQuery = request.URL.Path, request.URL.RawQuery
		_ = json.NewEncoder(writer).Encode(map[string]any{"scopes": []any{map[string]any{"scope": "test_first_waiver", "status": "collecting"}}})
	}))
	defer server.Close()
	test.Chdir(test.TempDir())
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mul_fake-member-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")

	command, _, err := rootCmd.Find([]string{"system1", "report"})
	if err != nil || command.Name() != "report" {
		test.Fatalf("system1 report not found: %v", err)
	}
	test.Cleanup(func() { _ = command.Flags().Set("scope", "") })
	out, err := captureStdout(test, func() error { return command.RunE(command, nil) })
	if err != nil {
		test.Fatal(err)
	}
	if gotPath != "/api/system1/report" || gotQuery != "" || !strings.Contains(out, `"status": "collecting"`) {
		test.Fatalf("path=%q query=%q out=%s", gotPath, gotQuery, out)
	}
	if err := command.Flags().Set("scope", "ci:cheese-work/multica:P1"); err != nil {
		test.Fatalf("--scope flag: %v", err)
	}
	if _, err := captureStdout(test, func() error { return command.RunE(command, nil) }); err != nil {
		test.Fatal(err)
	}
	if want := "scope=ci%3Acheese-work%2Fmultica%3AP1"; gotQuery != want {
		test.Fatalf("query = %q, want %q", gotQuery, want)
	}
}
