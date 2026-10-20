package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseChatWakeupAt(t *testing.T) {
	ict := time.FixedZone("ICT", 7*3600)
	now := time.Date(2026, 10, 20, 11, 15, 0, 0, ict)
	for _, tc := range []struct {
		in, want string
	}{
		{"11:40", "2026-10-20T11:40:00+07:00"},
		{"11:15", "2026-10-21T11:15:00+07:00"}, // not after now: next day
		{"09:00", "2026-10-21T09:00:00+07:00"},
		{"2026-10-20T12:00:00+07:00", "2026-10-20T12:00:00+07:00"},
	} {
		got, err := parseChatWakeupAt(tc.in, now, ict)
		if err != nil || got.Format(time.RFC3339) != tc.want {
			t.Errorf("%s = %v, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	if _, err := parseChatWakeupAt("noon", now, ict); err == nil {
		t.Error("accepted a time that is neither HH:MM nor RFC3339")
	}
}

func TestChatWakeupCreatePostsResolvedTimeAndInstruction(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	t.Chdir(t.TempDir())
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat/wakeups" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"w1","fire_at":"x","status":"pending"}`))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	const note = "检查部署\n保留换行"
	if err := os.WriteFile(filepath.Join(".", "instruction.md"), []byte(note), 0600); err != nil {
		t.Fatal(err)
	}

	chatWakeupCreateCmd.SetArgs(nil)
	for flag, value := range map[string]string{"after": "20m", "instruction-file": "./instruction.md"} {
		if err := chatWakeupCreateCmd.Flags().Set(flag, value); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		chatWakeupCreateCmd.Flags().Set("after", "")
		chatWakeupCreateCmd.Flags().Set("instruction-file", "")
	})
	if err := runChatWakeupCreate(chatWakeupCreateCmd, nil); err != nil {
		t.Fatal(err)
	}
	if body["after_seconds"] != float64(1200) || body["instruction"] != note || body["at"] != nil {
		t.Fatalf("unexpected body %+v", body)
	}

	// Both --at and --after is refused without a request.
	chatWakeupCreateCmd.Flags().Set("at", "11:40")
	t.Cleanup(func() { chatWakeupCreateCmd.Flags().Set("at", "") })
	if err := runChatWakeupCreate(chatWakeupCreateCmd, nil); err == nil {
		t.Fatal("accepted --at together with --after")
	}
}
