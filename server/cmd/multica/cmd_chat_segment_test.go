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

// Two real pages against a server that pages like the handler: before_created_at
// + before_id select older rows, next_cursor is an object. The CLI must turn the
// printed next_before into those two params, so page 2 differs from page 1.
func TestChatSegmentPagesThroughOlderMessagesWithTheReturnedCursor(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		seen = append(seen, r.URL.RawQuery)
		if q.Get("divider") != "d-1" || q.Get("limit") != "1" || q.Get("before") != "" {
			t.Errorf("unexpected request: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		if q.Get("before_created_at") == "" {
			_, _ = w.Write([]byte(`{"messages":[{"id":"m-2"}],"has_more":true,"next_cursor":{"created_at":"2026-11-01T10:00:02.5Z","id":"m-2"}}`))
			return
		}
		if q.Get("before_created_at") != "2026-11-01T10:00:02.5Z" || q.Get("before_id") != "m-2" {
			t.Errorf("page 2 cursor = %q %q", q.Get("before_created_at"), q.Get("before_id"))
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"m-1"}],"has_more":false}`))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)

	read := func(before string) string {
		var out bytes.Buffer
		command := chatSegmentTestCommand("json")
		command.SetOut(&out)
		_ = command.Flags().Set("limit", "1")
		if before != "" {
			_ = command.Flags().Set("before", before)
		}
		if err := runChatSegment(command, []string{"d-1"}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	page1 := read("")
	if !strings.Contains(page1, `"m-2"`) || !strings.Contains(page1, `"next_before": "2026-11-01T10:00:02.5Z,m-2"`) {
		t.Fatalf("page 1 lacks the message or next_before: %s", page1)
	}
	page2 := read("2026-11-01T10:00:02.5Z,m-2")
	if !strings.Contains(page2, `"m-1"`) || strings.Contains(page2, `"m-2"`) {
		t.Fatalf("page 2 repeats page 1 or misses the older message: %s", page2)
	}
	if len(seen) != 2 {
		t.Fatalf("requests = %v", seen)
	}
}

func TestChatSegmentRejectsMalformedBeforeWithoutHTTP(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("malformed --before reached the server: %s", r.URL)
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	for _, before := range []string{"nocomma", ",id", "2026-11-01T10:00:00Z,"} {
		command := chatSegmentTestCommand("json")
		_ = command.Flags().Set("before", before)
		if err := runChatSegment(command, []string{"d-1"}); err == nil {
			t.Fatalf("--before %q was accepted", before)
		}
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
