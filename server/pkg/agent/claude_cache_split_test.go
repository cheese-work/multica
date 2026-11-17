package agent

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func i64(v int64) *int64 { return &v }

// splitAssistant is a main-loop (or sub-agent, if parent != "") assistant event
// whose usage carries the cache_creation TTL split.
func splitAssistant(t *testing.T, id, model, parent string, h1, m5 int64) json.RawMessage {
	t.Helper()
	return mustMarshal(t, map[string]any{
		"type": "assistant", "parent_tool_use_id": parent,
		"message": map[string]any{"id": id, "role": "assistant", "model": model,
			"usage": map[string]any{
				"input_tokens": 1, "output_tokens": 1,
				"cache_creation_input_tokens": h1 + m5,
				"cache_creation": map[string]any{
					"ephemeral_1h_input_tokens": h1, "ephemeral_5m_input_tokens": m5,
				},
			},
			"content": splitContent(id)},
	})
}

// The fixture harness counts one text and one tool_use block per event.
func splitContent(id string) []any {
	return []any{
		map[string]any{"type": "text", "text": "x"},
		map[string]any{"type": "tool_use", "id": "tool_" + id, "name": "Read", "input": map[string]string{}},
	}
}

func TestClaudeCacheWrite1hSplit(t *testing.T) {
	const model = "claude-sonnet-4-6"
	const subModel = "claude-haiku-4-5"
	// Main loop: 80 + 0 one-hour of 200 written => ratio 0.4.
	mainEvents := func(t *testing.T) []json.RawMessage {
		return []json.RawMessage{
			splitAssistant(t, "m1", model, "", 80, 20),
			splitAssistant(t, "m2", model, "", 0, 100),
			splitAssistant(t, "m2", model, "", 0, 100), // duplicate id, counted once
		}
	}

	t.Run("modelUsage with sub-agent-only model", func(t *testing.T) {
		result := mustMarshal(t, map[string]any{
			"type": "result", "subtype": "success", "result": "done",
			"modelUsage": map[string]claudeResultModelUsage{
				model:    {InputTokens: 10, CacheCreationInputTokens: 300},
				subModel: {InputTokens: 5, CacheCreationInputTokens: 50},
			},
		})
		events := mainEvents(t)
		got := executeClaudeUsageFixture(t, claudeUsageFixtureBackend(t, events, result, false), len(events))
		if g := got.Usage[model].CacheWrite1hTokens; !reflect.DeepEqual(g, i64(120)) {
			t.Fatalf("%s 1h = %v, want 120 (300 x 0.4)", model, g)
		}
		if g := got.Usage[subModel].CacheWrite1hTokens; !reflect.DeepEqual(g, i64(20)) {
			t.Fatalf("%s 1h = %v, want 20 (50 x run-wide 0.4)", subModel, g)
		}
	})

	t.Run("result without modelUsage takes the main-loop sum", func(t *testing.T) {
		result := mustMarshal(t, map[string]any{
			"type": "result", "subtype": "success", "result": "done", "model": model,
			"usage": map[string]any{"input_tokens": 10, "cache_creation_input_tokens": 300},
		})
		events := mainEvents(t)
		got := executeClaudeUsageFixture(t, claudeUsageFixtureBackend(t, events, result, false), len(events))
		if g := got.Usage[model].CacheWrite1hTokens; !reflect.DeepEqual(g, i64(80)) {
			t.Fatalf("1h = %v, want 80 (main-loop sum)", g)
		}
	})

	t.Run("resumed run applies the ratio to the subtracted delta", func(t *testing.T) {
		const sessionID = "session-split"
		configDir := t.TempDir()
		writeClaudeUsageSession(t, configDir, sessionID,
			claudeCostStateFixture(t, map[string]claudeResultModelUsage{
				model: {InputTokens: 100, CacheCreationInputTokens: 40},
			}),
			[]byte(`{"type":"user","message":{"role":"user","content":"after baseline"}}`+"\n"),
		)
		result := mustMarshal(t, map[string]any{
			"type": "result", "subtype": "success", "session_id": sessionID, "result": "done",
			"modelUsage": map[string]claudeResultModelUsage{
				model: {InputTokens: 110, CacheCreationInputTokens: 240}, // delta 200
			},
		})
		events := mainEvents(t)
		backend := claudeUsageFixtureBackendWithEnv(t, events, result, false, map[string]string{"CLAUDE_CONFIG_DIR": configDir})
		got := executeClaudeUsageFixtureWithOptions(t, backend, len(events), ExecOptions{Timeout: 5 * time.Second, ResumeSessionID: sessionID})
		if w := got.Usage[model].CacheWriteTokens; w != 200 {
			t.Fatalf("cache write = %d, want delta 200", w)
		}
		if g := got.Usage[model].CacheWrite1hTokens; !reflect.DeepEqual(g, i64(80)) {
			t.Fatalf("1h = %v, want 80 (delta 200 x 0.4)", g)
		}
	})

	t.Run("no split field anywhere stays nil", func(t *testing.T) {
		plain := mustMarshal(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{"id": "p1", "role": "assistant", "model": model,
				"usage":   map[string]any{"input_tokens": 1, "cache_creation_input_tokens": 50},
				"content": splitContent("p1")},
		})
		result := mustMarshal(t, map[string]any{
			"type": "result", "subtype": "success", "result": "done",
			"modelUsage": map[string]claudeResultModelUsage{model: {InputTokens: 1, CacheCreationInputTokens: 50}},
		})
		got := executeClaudeUsageFixture(t, claudeUsageFixtureBackend(t, []json.RawMessage{plain}, result, false), 1)
		if g := got.Usage[model].CacheWrite1hTokens; g != nil {
			t.Fatalf("1h = %d, want nil (no split in run)", *g)
		}
	})
}
