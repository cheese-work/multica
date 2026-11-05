package daemon

import (
	"log/slog"
	"testing"
)

func TestManagedHarnessProfile(t *testing.T) {
	log := slog.Default()
	cases := []struct {
		name     string
		agent    *AgentData
		provider string
		want     string
	}{
		{"nil agent is personal", nil, "claude", ""},
		{"empty is personal", &AgentData{}, "claude", ""},
		{"personal stays personal", &AgentData{HarnessProfile: "personal"}, "codex", ""},
		{"managed claude", &AgentData{HarnessProfile: "managed"}, "claude", "managed"},
		{"managed codex", &AgentData{HarnessProfile: "managed"}, "codex", "managed"},
		{"managed unsupported provider falls back", &AgentData{HarnessProfile: "managed"}, "gemini", ""},
	}
	for _, c := range cases {
		if got := managedHarnessProfile(c.agent, c.provider, log); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
