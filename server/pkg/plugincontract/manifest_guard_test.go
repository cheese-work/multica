package plugincontract

import (
	"fmt"
	"strings"
	"testing"
)

// guardManifest rewrites the reference hook into a guard hook, then applies edit.
func guardManifest(t *testing.T, edit func(hook map[string]any)) []byte {
	t.Helper()
	return mutate(t, func(doc map[string]any) {
		hook := doc["contributes"].(map[string]any)["hooks"].([]any)[0].(map[string]any)
		hook["key"] = "qa-proof"
		hook["triggers"] = []any{"guard"}
		delete(hook, "timeout_ms")
		hook["guard"] = map[string]any{
			"timeout_ms":       2500,
			"on_error":         "closed",
			"overridable":      true,
			"metadata_writers": []any{"post_close_work"},
		}
		if edit != nil {
			edit(hook)
		}
	})
}

func TestParseManifestAcceptsGuardHook(t *testing.T) {
	manifest, canonical, err := ParseManifest(guardManifest(t, nil))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	hook := manifest.Contributes.Hooks[0]
	if hook.Guard == nil {
		t.Fatal("guard policy was dropped")
	}
	if hook.Guard.TimeoutMs != 2500 || hook.Guard.OnError != GuardOnErrorClosed || !hook.Guard.Overridable {
		t.Fatalf("guard = %+v", hook.Guard)
	}
	if len(hook.Guard.MetadataWriters) != 1 || hook.Guard.MetadataWriters[0] != "post_close_work" {
		t.Fatalf("metadata_writers = %v", hook.Guard.MetadataWriters)
	}
	// The stored snapshot must carry the policy: the host reads it back from
	// the installation row, never from the live manifest.
	if _, _, err := ParseManifest(canonical); err != nil {
		t.Fatalf("canonical manifest does not reparse: %v", err)
	}
	if !strings.Contains(string(canonical), `"on_error":"closed"`) {
		t.Fatalf("canonical manifest lost the guard policy: %s", canonical)
	}
}

func TestParseManifestAcceptsGuardBounds(t *testing.T) {
	keys := make([]any, 0, MaxGuardMetadataWriters)
	for i := 0; i < MaxGuardMetadataWriters; i++ {
		keys = append(keys, fmt.Sprintf("key_%d", i))
	}
	for name, edit := range map[string]func(map[string]any){
		"max timeout":      func(h map[string]any) { h["guard"].(map[string]any)["timeout_ms"] = 3000 },
		"on_error open":    func(h map[string]any) { h["guard"].(map[string]any)["on_error"] = "open" },
		"eight keys":       func(h map[string]any) { h["guard"].(map[string]any)["metadata_writers"] = keys },
		"no metadata keys": func(h map[string]any) { delete(h["guard"].(map[string]any), "metadata_writers") },
		"not overridable":  func(h map[string]any) { h["guard"].(map[string]any)["overridable"] = false },
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseManifest(guardManifest(t, edit)); err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
		})
	}
}

func TestParseManifestRefusesInvalidGuardHook(t *testing.T) {
	nine := make([]any, 0, 9)
	for i := 0; i < 9; i++ {
		nine = append(nine, fmt.Sprintf("key_%d", i))
	}
	policy := func(h map[string]any) map[string]any { return h["guard"].(map[string]any) }
	tests := []struct {
		name string
		edit func(hook map[string]any)
		want string
	}{
		{name: "timeout over 3000", edit: func(h map[string]any) { policy(h)["timeout_ms"] = 3001 }, want: "guard.timeout_ms"},
		{name: "timeout zero", edit: func(h map[string]any) { policy(h)["timeout_ms"] = 0 }, want: "guard.timeout_ms"},
		{name: "timeout missing", edit: func(h map[string]any) { delete(policy(h), "timeout_ms") }, want: "guard.timeout_ms"},
		{name: "timeout below floor", edit: func(h map[string]any) { policy(h)["timeout_ms"] = 99 }, want: "guard.timeout_ms"},
		{name: "unknown on_error", edit: func(h map[string]any) { policy(h)["on_error"] = "ignore" }, want: "guard.on_error"},
		{name: "missing on_error", edit: func(h map[string]any) { delete(policy(h), "on_error") }, want: "guard.on_error"},
		{name: "more than 8 keys", edit: func(h map[string]any) { policy(h)["metadata_writers"] = nine }, want: "guard.metadata_writers"},
		{name: "duplicate key", edit: func(h map[string]any) { policy(h)["metadata_writers"] = []any{"a", "a"} }, want: "duplicate"},
		{name: "invalid key", edit: func(h map[string]any) { policy(h)["metadata_writers"] = []any{"1 bad key"} }, want: "guard.metadata_writers"},
		{name: "guard plus manual", edit: func(h map[string]any) { h["triggers"] = []any{"guard", "manual"} }, want: "only trigger"},
		{name: "guard plus event", edit: func(h map[string]any) {
			h["triggers"] = []any{"event", "guard"}
			h["events"] = []any{"issue.created"}
		}, want: "only trigger"},
		{name: "guard plus schedule", edit: func(h map[string]any) {
			h["triggers"] = []any{"guard", "schedule"}
			h["schedule"] = map[string]any{"cron": "*/5 * * * *", "timezone": "UTC"}
		}, want: "only trigger"},
		{name: "guard block missing", edit: func(h map[string]any) { delete(h, "guard") }, want: "guard is required"},
		{name: "guard block without trigger", edit: func(h map[string]any) { h["triggers"] = []any{"manual"} }, want: "requires the guard trigger"},
		{name: "mcp transport", edit: func(h map[string]any) {
			h["transport"] = map[string]any{"type": "mcp", "url": "https://example.com/mcp"}
		}, want: "only supports the http transport"},
		{name: "hook timeout_ms alongside guard", edit: func(h map[string]any) { h["timeout_ms"] = 5000 }, want: "guard.timeout_ms"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseManifest(guardManifest(t, tc.edit))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
