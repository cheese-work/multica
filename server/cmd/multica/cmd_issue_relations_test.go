package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestIssueRelationsReturnsReverseEdges(t *testing.T) {
	chdirWithDaemonTaskMarker(t)
	const issueID = "11111111-1111-4111-8111-111111111111"
	response := map[string]any{
		"nodes": []any{map[string]any{"id": issueID}, map[string]any{"id": "waiting"}, map[string]any{"id": "mentioning"}},
		"edges": []any{
			map[string]any{"source": issueID, "target": "waiting", "type": "wakeup"},
			map[string]any{"source": "mentioning", "target": issueID, "type": "mention"},
		},
		"depth": float64(2), "omitted_count": float64(0),
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/api/issues/"+issueID+"/relations" || request.URL.Query().Get("depth") != "2" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("X-Workspace-ID") != "ws-1" {
			t.Error("missing workspace header")
		}
		_ = json.NewEncoder(writer).Encode(response)
	}))
	defer server.Close()
	setCLITestServerEnv(t, server.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	command, _, err := issueCmd.Find([]string{"relations"})
	if err != nil || command == issueCmd {
		t.Fatalf("issue relations command is unavailable: %v", err)
	}
	previousDepth := command.Flags().Lookup("depth").Value.String()
	if err := command.Flags().Set("depth", "2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Flags().Set("depth", previousDepth) })
	output, err := captureStdout(t, func() error { return command.RunE(command, []string{issueID}) })
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err := json.Unmarshal([]byte(output), &actual); err != nil {
		t.Fatalf("invalid JSON %q: %v", output, err)
	}
	if !reflect.DeepEqual(actual, response) || requests != 1 {
		t.Fatalf("relations lost reverse edges: %#v, requests=%d", actual, requests)
	}
}

func TestIssueRelationsRejectsInvalidDepth(t *testing.T) {
	previousDepth := issueRelationsCmd.Flags().Lookup("depth").Value.String()
	t.Cleanup(func() { _ = issueRelationsCmd.Flags().Set("depth", previousDepth) })
	for _, depth := range []string{"0", "3", "-1"} {
		if err := issueRelationsCmd.Flags().Set("depth", depth); err != nil {
			t.Fatal(err)
		}
		if err := runIssueRelations(issueRelationsCmd, []string{"CHE-1"}); err == nil || err.Error() != "depth must be 1 or 2" {
			t.Fatalf("depth %s: got %v", depth, err)
		}
	}
}
