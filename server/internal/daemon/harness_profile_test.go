package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestManagedHarnessProfile(t *testing.T) {
	cases := []struct {
		name     string
		agent    *AgentData
		provider string
		want     string
		wantErr  bool
	}{
		{"nil agent is personal", nil, "claude", "", false},
		{"empty is personal", &AgentData{}, "claude", "", false},
		{"personal stays personal", &AgentData{HarnessProfile: "personal"}, "codex", "", false},
		{"personal on any provider", &AgentData{HarnessProfile: "personal"}, "gemini", "", false},
		{"managed claude", &AgentData{HarnessProfile: "managed"}, "claude", "managed", false},
		{"managed codex", &AgentData{HarnessProfile: "managed"}, "codex", "managed", false},
		{"managed unsupported provider fails closed", &AgentData{HarnessProfile: "managed"}, "gemini", "", true},
		{"unknown profile fails closed", &AgentData{HarnessProfile: "locked-down"}, "claude", "", true},
	}
	for _, c := range cases {
		got, err := managedHarnessProfile(c.agent, c.provider)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("%s: got %q err=%v, want %q wantErr=%v", c.name, got, err, c.want, c.wantErr)
		}
	}
}

// Managed Claude with a null or absent agent MCP config must still get an
// explicit empty set, or buildClaudeArgs omits --strict-mcp-config and Claude
// discovers the user's own MCP servers (CHE-3706 review). pkg/agent's
// TestBuildClaudeArgsUsesStrictMCPForManagedConfig pins {} -> --strict-mcp-config.
func TestAgentMcpConfigForProfile_ManagedClaudeNullIsExplicitEmpty(t *testing.T) {
	for _, in := range []json.RawMessage{nil, json.RawMessage(``), json.RawMessage(`null`), json.RawMessage(" null \n")} {
		got, err := agentMcpConfigForProfile("claude", "managed", in)
		if err != nil || string(got) != `{}` {
			t.Errorf("managed claude %q: got %q err=%v, want {}", in, got, err)
		}
	}
	// Other providers and personal are unchanged.
	if got, _ := agentMcpConfigForProfile("codex", "managed", nil); got != nil {
		t.Errorf("managed codex null: got %q, want unchanged nil", got)
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

// The server hands managed tasks only to daemons advertising this capability.
func TestDaemonAdvertisesRuntimeProfileCapability(t *testing.T) {
	if !slices.Contains(daemonCommonCapabilities(), protocol.DaemonCapabilityRuntimeProfileV1) {
		t.Fatal("daemon must advertise runtime-profile-v1")
	}
}
