package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func system1TestEnv(test *testing.T, handler http.HandlerFunc) {
	server := httptest.NewServer(handler)
	test.Cleanup(server.Close)
	test.Chdir(test.TempDir())
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mul_fake-task-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
}

func system1SetFlags(test *testing.T, args []string, flags map[string]string) {
	command, _, err := rootCmd.Find(args)
	if err != nil || command.Name() != args[len(args)-1] {
		test.Fatalf("%s not found: %v", strings.Join(args, " "), err)
	}
	for name, value := range flags {
		if err := command.Flags().Set(name, value); err != nil {
			test.Fatalf("--%s flag: %v", name, err)
		}
	}
	test.Cleanup(func() {
		for name := range flags {
			_ = command.Flags().Set(name, "")
		}
	})
}

func TestSystem1LabelQueue(test *testing.T) {
	var gotMethod, gotPath, gotQuery string
	system1TestEnv(test, func(writer http.ResponseWriter, request *http.Request) {
		gotMethod, gotPath, gotQuery = request.Method, request.URL.Path, request.URL.RawQuery
		_ = json.NewEncoder(writer).Encode(map[string]any{"sample_id": "s-1", "items": []any{map[string]any{"observation_id": "o-1"}}})
	})
	system1SetFlags(test, []string{"system1", "label-queue"}, map[string]string{"scope": "test_first_waiver", "kind": "burn_in"})
	command, _, _ := rootCmd.Find([]string{"system1", "label-queue"})
	out, err := captureStdout(test, func() error { return command.RunE(command, nil) })
	if err != nil {
		test.Fatal(err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/system1/label-queue" || gotQuery != "kind=burn_in&scope=test_first_waiver" {
		test.Fatalf("called %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if !strings.Contains(out, `"observation_id": "o-1"`) {
		test.Fatalf("queue JSON not printed: %s", out)
	}
}

// A submit round-trips: the flags become the body of POST /api/system1/labels and the server's row is printed.
func TestSystem1LabelSubmit(test *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	system1TestEnv(test, func(writer http.ResponseWriter, request *http.Request) {
		gotMethod, gotPath = request.Method, request.URL.Path
		raw, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(raw, &gotBody)
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "l-1", "label": "waive"})
	})
	system1SetFlags(test, []string{"system1", "label", "submit"}, map[string]string{"observation": "o-1", "kind": "burn_in", "label": "waive"})
	command, _, _ := rootCmd.Find([]string{"system1", "label", "submit"})
	out, err := captureStdout(test, func() error { return command.RunE(command, nil) })
	if err != nil {
		test.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/system1/labels" {
		test.Fatalf("called %s %s", gotMethod, gotPath)
	}
	if gotBody["observation_id"] != "o-1" || gotBody["kind"] != "burn_in" || gotBody["label"] != "waive" {
		test.Fatalf("body = %v", gotBody)
	}
	if _, has := gotBody["supersedes"]; has {
		test.Fatalf("empty --supersedes must be omitted: %v", gotBody)
	}
	if !strings.Contains(out, `"id": "l-1"`) {
		test.Fatalf("created row not printed: %s", out)
	}
}
