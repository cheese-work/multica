package main

import (
	"context"
	"net/http"
	"testing"
)

type channelSwitches struct {
	InApp bool `json:"in_app"`
	Push  bool `json:"push"`
}

type channelsJSON struct {
	WorkspaceID string                     `json:"workspace_id"`
	Channels    map[string]channelSwitches `json:"channels"`
}

func resetNotificationChannelState(t *testing.T) {
	t.Helper()
	clean := func() {
		for _, table := range []string{"notification_channel_preference", "notification_preference"} {
			_, _ = testPool.Exec(context.Background(),
				`DELETE FROM `+table+` WHERE workspace_id = $1 AND user_id = $2`,
				testWorkspaceID, testUserID)
		}
	}
	clean()
	t.Cleanup(clean)
}

func getChannels(t *testing.T) channelsJSON {
	t.Helper()
	resp := authRequest(t, http.MethodGet, "/api/notification-channels", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/notification-channels: expected 200, got %d", resp.StatusCode)
	}
	var out channelsJSON
	readJSON(t, resp, &out)
	return out
}

func patchChannel(t *testing.T, body any, want int) channelsJSON {
	t.Helper()
	resp := authRequest(t, http.MethodPatch, "/api/notification-channels", body)
	if resp.StatusCode != want {
		t.Fatalf("PATCH /api/notification-channels %v: expected %d, got %d", body, want, resp.StatusCode)
	}
	var out channelsJSON
	if want == http.StatusOK {
		readJSON(t, resp, &out)
	} else {
		resp.Body.Close()
	}
	return out
}

func wantChannels(t *testing.T, got channelsJSON, want map[string]channelSwitches) {
	t.Helper()
	if len(got.Channels) != len(want) {
		t.Fatalf("channels = %+v, want %+v", got.Channels, want)
	}
	for source, sw := range want {
		if got.Channels[source] != sw {
			t.Fatalf("channels[%s] = %+v, want %+v (all: %+v)", source, got.Channels[source], sw, got.Channels)
		}
	}
}

var defaultChannels = map[string]channelSwitches{
	"inbox": {InApp: true, Push: false},
	"dm":    {InApp: true, Push: false},
}

func TestNotificationChannelsDefaults(t *testing.T) {
	resetNotificationChannelState(t)

	got := getChannels(t)
	if got.WorkspaceID != testWorkspaceID {
		t.Fatalf("workspace_id = %q, want %q", got.WorkspaceID, testWorkspaceID)
	}
	wantChannels(t, got, defaultChannels)
}

func TestNotificationChannelsPatchChangesOnlyOneKey(t *testing.T) {
	resetNotificationChannelState(t)

	got := patchChannel(t, map[string]any{"source": "dm", "channel": "push", "enabled": true}, http.StatusOK)
	wantChannels(t, got, map[string]channelSwitches{
		"inbox": {InApp: true, Push: false},
		"dm":    {InApp: true, Push: true},
	})

	// The same key twice is idempotent, and a second key leaves the first alone.
	patchChannel(t, map[string]any{"source": "dm", "channel": "push", "enabled": true}, http.StatusOK)
	got = patchChannel(t, map[string]any{"source": "inbox", "channel": "in_app", "enabled": false}, http.StatusOK)
	want := map[string]channelSwitches{
		"inbox": {InApp: false, Push: false},
		"dm":    {InApp: true, Push: true},
	}
	wantChannels(t, got, want)
	wantChannels(t, getChannels(t), want)
}

func TestNotificationChannelsSurviveLegacyReplaceAllPut(t *testing.T) {
	resetNotificationChannelState(t)

	want := map[string]channelSwitches{
		"inbox": {InApp: false, Push: true},
		"dm":    {InApp: false, Push: true},
	}
	for source := range want {
		patchChannel(t, map[string]any{"source": source, "channel": "in_app", "enabled": false}, http.StatusOK)
		patchChannel(t, map[string]any{"source": source, "channel": "push", "enabled": true}, http.StatusOK)
	}

	// An old client replaces the whole legacy map.
	put := authRequest(t, http.MethodPut, "/api/notification-preferences", map[string]any{
		"preferences": map[string]string{"comments": "muted"},
	})
	if put.StatusCode != http.StatusOK {
		t.Fatalf("legacy PUT: expected 200, got %d", put.StatusCode)
	}
	put.Body.Close()

	wantChannels(t, getChannels(t), want)

	// The legacy map never carries the channel switches.
	resp := authRequest(t, http.MethodGet, "/api/notification-preferences", nil)
	var legacy struct {
		Preferences map[string]string `json:"preferences"`
	}
	readJSON(t, resp, &legacy)
	if len(legacy.Preferences) != 1 || legacy.Preferences["comments"] != "muted" {
		t.Fatalf("legacy preferences = %+v, want only comments=muted", legacy.Preferences)
	}
}

func TestNotificationChannelsPatchRejectsMalformedBodies(t *testing.T) {
	resetNotificationChannelState(t)

	cases := map[string]any{
		"unknown source":     map[string]any{"source": "email", "channel": "push", "enabled": true},
		"unknown channel":    map[string]any{"source": "dm", "channel": "sms", "enabled": true},
		"missing source":     map[string]any{"channel": "push", "enabled": true},
		"missing channel":    map[string]any{"source": "dm", "enabled": true},
		"missing enabled":    map[string]any{"source": "dm", "channel": "push"},
		"non-bool enabled":   map[string]any{"source": "dm", "channel": "push", "enabled": "yes"},
		"null enabled":       map[string]any{"source": "dm", "channel": "push", "enabled": nil},
		"not an object":      `["dm","push",true]`,
		"invalid json":       `{"source":`,
		"legacy-shaped body": map[string]any{"preferences": map[string]string{"comments": "muted"}},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			patchChannel(t, body, http.StatusBadRequest)
		})
	}

	// Nothing was written by the rejected requests.
	wantChannels(t, getChannels(t), defaultChannels)
}

func TestNotificationChannelsRequireAuth(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		req, err := http.NewRequest(method, testServer.URL+"/api/notification-channels", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without token: expected 401, got %d", method, resp.StatusCode)
		}
	}
}
