package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func chatSegmentTestCommand(output string) *cobra.Command {
	c := &cobra.Command{Use: "segment"}
	c.Flags().Int("limit", 0, "")
	c.Flags().String("before", "", "")
	c.Flags().String("output", output, "")
	return c
}

func TestChatSegmentsListsDividersFromTheOwnDMEndpoint(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/chat/segments" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"segments":[{"divider_id":"d-1","created_at":"2026-11-01T10:00:00Z","message_count":6}]}`))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	var out bytes.Buffer
	command := chatSegmentTestCommand("table")
	command.SetOut(&out)
	if err := runChatSegments(command, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "d-1") || !strings.Contains(got, "6") {
		t.Fatalf("table misses the divider row: %q", got)
	}
}

func TestChatSegmentReadsOneSegmentWithPaging(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/chat/segment" || q.Get("divider") != "d-1" || q.Get("limit") != "5" || q.Get("before") != "cur" {
			t.Errorf("unexpected request: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"id":"m-1"}],"has_more":false}`))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	var out bytes.Buffer
	command := chatSegmentTestCommand("json")
	command.SetOut(&out)
	_ = command.Flags().Set("limit", "5")
	_ = command.Flags().Set("before", "cur")
	if err := runChatSegment(command, []string{"d-1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"m-1"`) {
		t.Fatalf("missing messages: %s", out.String())
	}
}

func TestChatSegmentUnknownDividerFailsWithServerReason(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"segment not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	var out bytes.Buffer
	command := chatSegmentTestCommand("json")
	command.SetOut(&out)
	err := runChatSegment(command, []string{"nope"})
	if err == nil || !strings.Contains(err.Error(), "segment not found") {
		t.Fatalf("want a segment-not-found error, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("error path printed output: %q", out.String())
	}
}
