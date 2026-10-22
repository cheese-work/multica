package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newAgentSkillsRuntimeSetTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "runtime-set"}
	registerAgentSkillsRuntimeSetFlags(c)
	c.Flags().String("profile", "", "")
	return c
}

// runtimeSkillMockServer captures the PUT body and serves the agent readback.
func runtimeSkillMockServer(t *testing.T, putStatus int, gotBody *map[string]any, puts *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/agents/agent-1/runtime-skills/enabled":
			*puts++
			if err := json.NewDecoder(r.Body).Decode(gotBody); err != nil {
				t.Errorf("decode put body: %v", err)
			}
			if putStatus != http.StatusNoContent {
				w.WriteHeader(putStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "agent is no longer assigned to this runtime"})
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/agents/agent-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "agent-1",
				"disabled_runtime_skills": []any{map[string]any{
					"runtime_id": "runtime-1", "provider": "claude", "root": "provider",
					"key": "gsd-add-tests", "name": "gsd-add-tests",
				}},
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestAgentSkillsRuntimeSetSendsDisable(t *testing.T) {
	var gotBody map[string]any
	puts := 0
	srv := runtimeSkillMockServer(t, http.StatusNoContent, &gotBody, &puts)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd := newAgentSkillsRuntimeSetTestCmd()
	for k, v := range map[string]string{
		"runtime-id": "runtime-1", "root": "provider", "key": "gsd-add-tests",
		"name": "gsd-add-tests", "enabled": "false",
	} {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set --%s: %v", k, err)
		}
	}
	if err := runAgentSkillsRuntimeSet(cmd, []string{"agent-1"}); err != nil {
		t.Fatalf("runAgentSkillsRuntimeSet: %v", err)
	}
	if puts != 1 {
		t.Fatalf("PUT calls = %d, want 1", puts)
	}
	want := map[string]any{
		"runtime_id": "runtime-1", "root": "provider", "key": "gsd-add-tests",
		"name": "gsd-add-tests", "plugin": "", "enabled": false,
	}
	if !reflect.DeepEqual(gotBody, want) {
		t.Errorf("PUT body = %v, want %v", gotBody, want)
	}
}

func TestAgentSkillsRuntimeSetRequiresIdentityAndEnabled(t *testing.T) {
	var gotBody map[string]any
	puts := 0
	srv := runtimeSkillMockServer(t, http.StatusNoContent, &gotBody, &puts)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	for _, missing := range []string{"runtime-id", "root", "key", "enabled"} {
		cmd := newAgentSkillsRuntimeSetTestCmd()
		for k, v := range map[string]string{
			"runtime-id": "runtime-1", "root": "provider", "key": "gsd-add-tests", "enabled": "false",
		} {
			if k != missing {
				_ = cmd.Flags().Set(k, v)
			}
		}
		err := runAgentSkillsRuntimeSet(cmd, []string{"agent-1"})
		if err == nil || !strings.Contains(err.Error(), "--"+missing) {
			t.Errorf("missing --%s: err = %v, want it named", missing, err)
		}
	}
	if puts != 0 {
		t.Errorf("PUT calls = %d, want 0 when a required flag is missing", puts)
	}
}

func TestAgentSkillsRuntimeSetSurfacesServerRejection(t *testing.T) {
	var gotBody map[string]any
	puts := 0
	srv := runtimeSkillMockServer(t, http.StatusConflict, &gotBody, &puts)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd := newAgentSkillsRuntimeSetTestCmd()
	for k, v := range map[string]string{
		"runtime-id": "runtime-2", "root": "provider", "key": "gsd-add-tests", "enabled": "false",
	} {
		_ = cmd.Flags().Set(k, v)
	}
	err := runAgentSkillsRuntimeSet(cmd, []string{"agent-1"})
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Errorf("err = %v, want the 409 surfaced", err)
	}
	if puts != 1 {
		t.Errorf("PUT calls = %d, want exactly 1 (no retry)", puts)
	}
}
