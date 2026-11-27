package daemon

import "testing"

// CHE-9773: the daemon must hand a binding spec to Claude-runtime launches
// exactly as it does for Codex, and to no other provider.
func TestRunBindingSpecByProvider(t *testing.T) {
	task := Task{ID: "task-1", AgentID: "agent-1", IssueIdentifier: "CHE-9773"}
	for _, provider := range []string{"codex", "claude"} {
		spec := runBindingSpec(provider, "/reg", task)
		if spec == nil || spec.Dir != "/reg" || spec.AgentID != "agent-1" || spec.TaskID != "task-1" || spec.IssueID != "CHE-9773" {
			t.Fatalf("%s: unexpected spec %+v", provider, spec)
		}
		if runBindingSpec(provider, "", task) != nil {
			t.Fatalf("%s: spec without a registry dir", provider)
		}
		if runBindingSpec(provider, "/reg", Task{ID: "t", AgentID: "a"}) != nil {
			t.Fatalf("%s: spec without an issue identity", provider)
		}
	}
	if runBindingSpec("gemini", "/reg", task) != nil {
		t.Fatal("unrelated provider got a binding")
	}
}
