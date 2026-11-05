package daemon

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
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

// A managed task gets only the MCP servers assigned to the agent. Personal
// runtime servers, and their credentials, stay out, also for an explicit empty
// or populated agent config (CHE-3706 review).
func TestAgentMcpConfigForProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"),
		[]byte(`{"mcpServers":{"personal":{"command":"personal-server"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	servers := func(raw json.RawMessage) map[string]any {
		var doc struct {
			McpServers map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		return doc.McpServers
	}
	cases := []struct {
		name, profile, agent string
		wantPersonal         bool
		wantAssigned         bool
	}{
		{"managed empty set", "managed", `{"mcpServers":{}}`, false, false},
		{"managed populated", "managed", `{"mcpServers":{"assigned":{"command":"a"}}}`, false, true},
		{"personal empty set keeps today's merge", "", `{"mcpServers":{}}`, true, false},
		{"personal populated keeps today's merge", "", `{"mcpServers":{"assigned":{"command":"a"}}}`, true, true},
	}
	for _, c := range cases {
		got, err := agentMcpConfigForProfile("claude", c.profile, json.RawMessage(c.agent))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		s := servers(got)
		if _, ok := s["personal"]; ok != c.wantPersonal {
			t.Errorf("%s: personal server present=%v want %v (%v)", c.name, ok, c.wantPersonal, s)
		}
		if _, ok := s["assigned"]; ok != c.wantAssigned {
			t.Errorf("%s: assigned server present=%v want %v (%v)", c.name, ok, c.wantAssigned, s)
		}
	}
}
