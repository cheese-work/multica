package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// CHE-2915 (DX-6): --compact-agent is an alias over `background-agents set compact`.
const (
	caTargetWS = "aaaaaaaa-0000-0000-0000-000000000001"
	caOtherWS  = "bbbbbbbb-0000-0000-0000-000000000002"
	caAgentA   = "11111111-1111-1111-1111-111111111111"
	caAgentB   = "22222222-2222-2222-2222-222222222222"
	caAgentC   = "33333333-3333-3333-3333-333333333333"
)

type caFake struct {
	mu         sync.Mutex
	store      map[string]any // what GET /background-agents returns
	puts       int
	dropWrites bool     // a PUT is accepted but not applied: read-back sees the old value
	agentWS    []string // workspace_id of each agent lookup
}

func newCAFake(t *testing.T, store map[string]any) *caFake {
	t.Helper()
	clearAmbientDaemonTaskEnv(t)
	f := &caFake{store: store}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		bg := "/api/workspaces/" + caTargetWS + "/background-agents"
		switch {
		case r.URL.Path == "/api/agents":
			f.agentWS = append(f.agentWS, r.URL.Query().Get("workspace_id"))
			io.WriteString(w, `[{"id":"`+caAgentA+`","name":"Terra Haiku"},{"id":"`+caAgentB+`","name":"Terra Sonnet"},{"id":"`+caAgentC+`","name":"Luna"}]`)
		case r.URL.Path == bg && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(f.store)
		case r.URL.Path == bg && r.Method == http.MethodPut:
			f.puts++
			var next map[string]any
			_ = json.NewDecoder(r.Body).Decode(&next)
			if !f.dropWrites {
				f.store = next
			}
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", caOtherWS) // the ambient workspace is NOT the target
	return f
}

func runCompactAgent(t *testing.T, value string, extra ...string) error {
	t.Helper()
	resetWorkspaceUpdateFlags(t)
	setStringFlag(t, "compact-agent", value)
	for i := 0; i+1 < len(extra); i += 2 {
		setStringFlag(t, extra[i], extra[i+1])
	}
	return runWorkspaceUpdate(workspaceUpdateCmd, []string{caTargetWS})
}

func (f *caFake) compact() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, _ := f.store["compact"].(map[string]any)
	return c
}

func TestCompactAgentSetByID(t *testing.T) {
	f := newCAFake(t, map[string]any{"dm_title": map[string]any{"agent_id": caAgentC}})
	if err := runCompactAgent(t, caAgentB); err != nil {
		t.Fatal(err)
	}
	if got := f.compact()["agent_id"]; got != caAgentB {
		t.Fatalf("compact = %v, want agent %s", f.compact(), caAgentB)
	}
	if f.store["dm_title"] == nil {
		t.Fatalf("another job was dropped: %v", f.store)
	}
}

func TestCompactAgentSetByNameInTheTargetWorkspace(t *testing.T) {
	f := newCAFake(t, map[string]any{})
	if err := runCompactAgent(t, "Luna"); err != nil {
		t.Fatal(err)
	}
	if got := f.compact()["agent_id"]; got != caAgentC {
		t.Fatalf("compact = %v, want agent %s", f.compact(), caAgentC)
	}
	if len(f.agentWS) == 0 || f.agentWS[0] != caTargetWS {
		t.Fatalf("agent lookup used workspace %v, want the target %s", f.agentWS, caTargetWS)
	}
}

func TestCompactAgentResetWithNullKeepsOtherJobs(t *testing.T) {
	f := newCAFake(t, map[string]any{
		"compact":  map[string]any{"agent_id": caAgentA},
		"dm_title": map[string]any{"agent_id": caAgentC},
	})
	if err := runCompactAgent(t, "null"); err != nil {
		t.Fatal(err)
	}
	if c := f.compact(); c["agent_id"] != nil {
		t.Fatalf("compact agent not reset: %v", c)
	}
	if f.store["dm_title"] == nil {
		t.Fatalf("another job was dropped: %v", f.store)
	}
}

func TestCompactAgentAmbiguousNameWritesNothing(t *testing.T) {
	f := newCAFake(t, map[string]any{})
	err := runCompactAgent(t, "Terra")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want an ambiguous-name error", err)
	}
	if f.puts != 0 {
		t.Fatalf("%d writes after an ambiguous name", f.puts)
	}
}

func TestCompactAgentReadBackMismatchIsActionable(t *testing.T) {
	f := newCAFake(t, map[string]any{})
	f.dropWrites = true
	err := runCompactAgent(t, caAgentB)
	if err == nil {
		t.Fatal("a write the server did not keep reported success")
	}
	for _, want := range []string{"was not saved", caAgentB, "background-agents list"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, missing %q", err, want)
		}
	}
}

func TestCompactAgentIsAnExclusiveSingleSettingGroup(t *testing.T) {
	f := newCAFake(t, map[string]any{})
	for _, other := range []string{"name", "routing-agent", "compact-prompt-file", "ask-guard-mode"} {
		err := runCompactAgent(t, caAgentA, other, "x")
		if err == nil || !strings.Contains(err.Error(), "--compact-agent cannot be combined with --"+other) {
			t.Fatalf("with --%s: err = %v, want a conflict naming both flags", other, err)
		}
	}
	if f.puts != 0 {
		t.Fatalf("%d writes after a refused combination", f.puts)
	}
}
