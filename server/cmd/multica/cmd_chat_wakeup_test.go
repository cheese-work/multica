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

func TestChatWakeupCreateConditionFlagsBuildTheConditionBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags map[string]string
		want  map[string]any
		fail  bool
	}{
		{"status", map[string]string{"until-issue": "CHE-12", "until-status": "done"}, map[string]any{"issue": "CHE-12", "status": "done"}, false},
		{"pr checks", map[string]string{"until-issue": "CHE-12", "until-pr": "checks"}, map[string]any{"issue": "CHE-12", "pr": "checks_finished"}, false},
		{"pr merged", map[string]string{"until-issue": "CHE-12", "until-pr": "merged"}, map[string]any{"issue": "CHE-12", "pr": "merged"}, false},
		{"comment", map[string]string{"until-issue": "CHE-12", "event": "comment.created", "filter-actor-id": "u1"},
			map[string]any{"issue": "CHE-12", "comment_by": map[string]any{"type": "member", "id": "u1"}}, false},
		{"agent comment", map[string]string{"until-issue": "CHE-12", "event": "comment.created", "filter-actor-id": "a1", "filter-actor-type": "agent"},
			map[string]any{"issue": "CHE-12", "comment_by": map[string]any{"type": "agent", "id": "a1"}}, false},
		{"issue alone", map[string]string{"until-issue": "CHE-12"}, nil, true},
		{"two predicates", map[string]string{"until-issue": "CHE-12", "until-status": "done", "until-pr": "merged"}, nil, true},
		{"predicate without issue", map[string]string{"until-status": "done"}, nil, true},
		{"unknown pr", map[string]string{"until-issue": "CHE-12", "until-pr": "opened"}, nil, true},
		{"unknown event", map[string]string{"until-issue": "CHE-12", "event": "issue.updated"}, nil, true},
		{"comment without actor", map[string]string{"until-issue": "CHE-12", "event": "comment.created"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAmbientDaemonTaskEnv(t)
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"w1","fire_at":"x","status":"pending"}`))
			}))
			t.Cleanup(server.Close)
			setCLITestServerEnv(t, server.URL)
			flags := map[string]string{"after": "1h", "instruction": "check it"}
			for k, v := range tc.flags {
				flags[k] = v
			}
			for k, v := range flags {
				if err := chatWakeupCreateCmd.Flags().Set(k, v); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				for k := range flags {
					chatWakeupCreateCmd.Flags().Set(k, "")
				}
			})
			err := runChatWakeupCreate(chatWakeupCreateCmd, nil)
			if tc.fail {
				if err == nil || body != nil {
					t.Fatalf("want a refusal before any request, got err=%v body=%v", err, body)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(body["condition"])
			want, _ := json.Marshal(tc.want)
			if string(got) != string(want) || body["after_seconds"] != float64(3600) {
				t.Fatalf("condition = %s, want %s (body %v)", got, want, body)
			}
		})
	}
}
