package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// CHE-4151 (L7): `multica chat attention` and `multica chat later` (plan r4 acceptance A2, A13, A14, C1).

func attentionRows() []map[string]any {
	return []map[string]any{
		{"id": "dm-fresh", "title": "Fresh", "stale_level": "fresh", "attention_source": "ask", "attention_since": "2026-11-07T08:00:00Z"},
		{"id": "dm-none", "title": "Quiet", "stale_level": "none", "attention_source": nil, "attention_since": nil},
		{"id": "dm-red", "title": "Red", "stale_level": "red", "attention_source": "decision", "attention_since": "2026-11-05T08:00:00Z"},
		{"id": "dm-yellow", "title": "Yellow", "stale_level": "yellow", "attention_source": "ask", "attention_since": "2026-11-07T01:00:00Z"},
		{"id": "dm-archived", "title": "Archived", "status": "archived", "stale_level": "red", "attention_source": "ask", "attention_since": "2026-11-04T08:00:00Z"},
		// A future `later` and an archived chain read stale_level "none" from the server projection (C1, T14).
		{"id": "dm-future-later", "title": "Later", "stale_level": "none", "attention_source": nil, "attention_since": nil},
	}
}

func runAttention(t *testing.T, stale bool) []map[string]any {
	t.Helper()
	command, _, err := chatCmd.Find([]string{"attention"})
	if err != nil || command == chatCmd {
		t.Fatal("chat attention command is missing")
	}
	useChatTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/chat/sessions" || r.URL.Query().Get("status") == "all" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		_ = json.NewEncoder(w).Encode(attentionRows())
	})
	_ = command.Flags().Set("output", "json")
	_ = command.Flags().Set("stale", map[bool]string{true: "true", false: "false"}[stale])
	out, err := captureStdout(t, func() error { return command.RunE(command, nil) })
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	return rows
}

func ids(rows []map[string]any) string {
	var s []string
	for _, r := range rows {
		s = append(s, r["id"].(string))
	}
	return strings.Join(s, ",")
}

func TestChatAttentionListsOldestFirstWithoutQuietChains(t *testing.T) {
	// none rows (no obligation, future later, archived chain) are absent; oldest attention_since first.
	if got := ids(runAttention(t, false)); got != "dm-red,dm-yellow,dm-fresh" {
		t.Fatalf("order/filter = %s", got)
	}
}

func TestChatAttentionStaleKeepsYellowAndRed(t *testing.T) {
	if got := ids(runAttention(t, true)); got != "dm-red,dm-yellow" {
		t.Fatalf("--stale = %s", got)
	}
}

func TestChatLaterSnoozesDisplayedKeys(t *testing.T) {
	command, _, err := chatCmd.Find([]string{"later"})
	if err != nil || command == chatCmd {
		t.Fatal("chat later command is missing")
	}
	var posted map[string]any
	useChatTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/chat/sessions/dm-1/attention":
			_ = json.NewEncoder(w).Encode(map[string]any{"attention": map[string]any{"keys": []string{"item:a", "decision:b@1"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/chat/sessions/dm-1/attention/later":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &posted)
			_ = json.NewEncoder(w).Encode(map[string]any{"attention": map[string]any{}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	})
	_ = command.Flags().Set("request-id", "11111111-1111-4111-8111-111111111111")
	if _, err := captureStdout(t, func() error { return command.RunE(command, []string{"dm-1", "90m"}) }); err != nil {
		t.Fatal(err)
	}
	keys, _ := posted["subject_keys"].([]any)
	if len(keys) != 2 || posted["duration_seconds"] != float64(5400) || posted["request_id"] != "11111111-1111-4111-8111-111111111111" || posted["until"] != nil {
		t.Fatalf("wrong body: %v", posted)
	}
}

func TestChatLaterDurationParsing(t *testing.T) {
	for in, want := range map[string]int64{"2h": 7200, "30m": 1800, "7d": 604800, "1d12h": 129600, "45": 45} {
		got, err := parseLaterSeconds(in)
		if err != nil || got != want {
			t.Errorf("parseLaterSeconds(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "99999999999999999999d", "9999999999d"} {
		if got, err := parseLaterSeconds(bad); err == nil {
			t.Errorf("parseLaterSeconds(%q) accepted as %d", bad, got)
		}
	}
}

// A13: out-of-range values are sent as-is and the server's range message reaches the user; no silent cap.
func TestChatLaterSurfacesServerRangeError(t *testing.T) {
	command, _, _ := chatCmd.Find([]string{"later"})
	if command == nil || command == chatCmd {
		t.Fatal("chat later command is missing")
	}
	var body map[string]any
	useChatTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"attention": map[string]any{"keys": []string{}}})
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		http.Error(w, `{"error":"later must be between 1s and 168h from now"}`, http.StatusUnprocessableEntity)
	})
	for _, in := range []string{"30d", "0", "-1h"} {
		_, err := captureStdout(t, func() error { return command.RunE(command, []string{"dm-1", in}) })
		if err == nil || !strings.Contains(err.Error(), "between 1s and 168h") {
			t.Fatalf("%s: range error not surfaced: %v", in, err)
		}
	}
	if body["duration_seconds"] != float64(-3600) {
		t.Fatalf("-1h was altered before send: %v", body)
	}
}

func TestChatLaterDefaultsAndUntil(t *testing.T) {
	command, _, _ := chatCmd.Find([]string{"later"})
	if command == nil || command == chatCmd {
		t.Fatal("chat later command is missing")
	}
	var body map[string]any
	useChatTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"attention": map[string]any{}})
			return
		}
		b, _ := io.ReadAll(r.Body)
		body = nil
		_ = json.Unmarshal(b, &body)
		_ = json.NewEncoder(w).Encode(map[string]any{"attention": map[string]any{}})
	})
	if _, err := captureStdout(t, func() error { return command.RunE(command, []string{"dm-1"}) }); err != nil {
		t.Fatal(err)
	}
	if body["duration_seconds"] != nil || body["until"] != nil {
		t.Fatalf("no duration must let the server default to 2h: %v", body)
	}
	if _, err := captureStdout(t, func() error { return command.RunE(command, []string{"dm-1", "2026-11-08T09:00:00+07:00"}) }); err != nil {
		t.Fatal(err)
	}
	if body["until"] != "2026-11-08T09:00:00+07:00" || body["duration_seconds"] != nil {
		t.Fatalf("until not sent as given: %v", body)
	}
}
