package service

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestShouldAutoCompact(t *testing.T) {
	for _, testCase := range []struct {
		name, setting                       string
		input, cached, written, window, cli int64
		want                                bool
	}{
		{"off", "off", 180000, 0, 0, 200000, 190000, false},
		{"default off", "", 180000, 0, 0, 200000, 190000, false},
		{"at boundary", "threshold:80", 150000, 5000, 5000, 200000, 190000, true},
		{"one token below", "threshold:80", 150000, 5000, 4999, 200000, 190000, false},
		{"one million window", "threshold:80", 10000, 800000, 0, 1000000, 950000, true},
		{"unknown window", "threshold:80", 180000, 0, 0, 0, 190000, false},
		{"CLI override below threshold", "threshold:80", 180000, 0, 0, 200000, 150000, false},
		{"CLI point equal threshold", "threshold:80", 180000, 0, 0, 200000, 160000, false},
		{"negative tokens", "threshold:80", -1, 180000, 0, 200000, 190000, false},
		{"invalid setting", "threshold:100", 180000, 0, 0, 200000, 190000, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config, _ := json.Marshal(map[string]string{"auto_compact": testCase.setting})
			request := &agent.ModelRequestUsage{Provider: "claude", Model: "claude-sonnet-4-6", InputTokens: testCase.input, CacheReadTokens: testCase.cached, CacheWriteTokens: testCase.written, ContextWindow: testCase.window, CLICompactionTokens: testCase.cli}
			if got := ShouldAutoCompact(config, request); got != testCase.want {
				t.Fatalf("decision = %v, want %v", got, testCase.want)
			}
		})
	}
	if ShouldAutoCompact([]byte(`{"auto_compact":"threshold:80"}`), nil) {
		t.Fatal("missing last request enabled auto compact")
	}
}

func TestValidateAutoCompactConfig(t *testing.T) {
	for _, value := range []any{true, "threshold:0", "threshold:90", "threshold:1.5", "threshold:abc", "jev"} {
		if err := ValidateAutoCompactConfig(map[string]any{"auto_compact": value}); err == nil {
			t.Fatalf("invalid setting accepted: %v", value)
		}
	}
	for _, value := range []any{nil, "", "off", "threshold:80", "threshold:1", "threshold:89"} {
		if err := ValidateAutoCompactConfig(map[string]any{"auto_compact": value}); err != nil {
			t.Fatalf("valid setting rejected: %v: %v", value, err)
		}
	}
}
