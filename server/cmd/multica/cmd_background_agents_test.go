package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

const (
	baWS     = "00000000-0000-0000-0000-0000000000ws"
	baAgentA = "11111111-1111-1111-1111-111111111111"
	baAgentB = "22222222-2222-2222-2222-222222222222"
	baAgentC = "33333333-3333-3333-3333-333333333333"
)

type baRequest struct{ Method, Path, Body string }

// baServer fakes the workspace and member routes plus the agent list. The
// store maps are what GET returns; writes are recorded, not applied.
func baServer(t *testing.T, workspace, mine string, writeStatus int, writeBody string) (*[]baRequest, func()) {
	t.Helper()
	var mu sync.Mutex
	var reqs []baRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, baRequest{r.Method, r.URL.Path, string(b)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/agents":
			io.WriteString(w, `[{"id":"`+baAgentA+`","name":"Terra Haiku"},{"id":"`+baAgentB+`","name":"Terra Sonnet"},{"id":"`+baAgentC+`","name":"Luna"}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/"+baWS+"/background-agents":
			io.WriteString(w, workspace)
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/"+baWS+"/me/background-agents":
			io.WriteString(w, mine)
		default:
			w.WriteHeader(writeStatus)
			io.WriteString(w, writeBody)
		}
	}))
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_TOKEN", "test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", baWS)
	return &reqs, srv.Close
}

func baRun(t *testing.T, cmd *cobra.Command, args []string, flags map[string]string) (string, error) {
	t.Helper()
	for _, name := range []string{"agent", "fallback", "output"} {
		if f := cmd.Flags().Lookup(name); f != nil {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}
	for k, v := range flags {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	return captureStdout(t, func() error { return cmd.RunE(cmd, args) })
}

func baWrite(t *testing.T, reqs *[]baRequest) baRequest {
	t.Helper()
	for _, r := range *reqs {
		if r.Method != http.MethodGet {
			return r
		}
	}
	t.Fatalf("no write was sent; requests: %v", *reqs)
	return baRequest{}
}

func TestMeSetNoneSendsClearedField(t *testing.T) {
	reqs, closeSrv := baServer(t, `{}`, `{}`, 200, `{}`)
	defer closeSrv()
	if _, err := baRun(t, meBackgroundAgentsSetCmd, []string{"dm_title"}, map[string]string{"agent": "none"}); err != nil {
		t.Fatal(err)
	}
	w := baWrite(t, reqs)
	if w.Method != http.MethodPut || w.Path != "/api/workspaces/"+baWS+"/me/background-agents" {
		t.Fatalf("write = %+v", w)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal([]byte(w.Body), &got); err != nil {
		t.Fatal(err)
	}
	v, present := got["dm_title"]["agent_id"]
	if !present || v != nil || len(got["dm_title"]) != 1 {
		t.Fatalf("body = %s, want only dm_title.agent_id:null", w.Body)
	}
}

func TestMeSetResolvesNameAndSetsFallback(t *testing.T) {
	reqs, closeSrv := baServer(t, `{}`, `{}`, 200, `{}`)
	defer closeSrv()
	if _, err := baRun(t, meBackgroundAgentsSetCmd, []string{"quick_actions"}, map[string]string{"agent": "luna", "fallback": baAgentA}); err != nil {
		t.Fatal(err)
	}
	if body := baWrite(t, reqs).Body; !strings.Contains(body, baAgentC) || !strings.Contains(body, baAgentA) {
		t.Fatalf("body = %s", body)
	}
}

func TestSetRefusesAmbiguousNameAndWritesNothing(t *testing.T) {
	for _, cmd := range []*cobra.Command{meBackgroundAgentsSetCmd, workspaceBackgroundAgentsSetCmd} {
		reqs, closeSrv := baServer(t, `{}`, `{}`, 200, `{}`)
		_, err := baRun(t, cmd, []string{"dm_title"}, map[string]string{"agent": "terra"})
		closeSrv()
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("%s: err = %v, want ambiguous", cmd.CommandPath(), err)
		}
		for _, r := range *reqs {
			if r.Method != http.MethodGet {
				t.Fatalf("write sent despite ambiguity: %+v", r)
			}
		}
	}
}

func TestSetRejectsUnknownJobAndMissingFlags(t *testing.T) {
	_, closeSrv := baServer(t, `{}`, `{}`, 200, `{}`)
	defer closeSrv()
	if _, err := baRun(t, workspaceBackgroundAgentsSetCmd, []string{"nope"}, map[string]string{"agent": "none"}); err == nil {
		t.Fatal("unknown job accepted")
	}
	if _, err := baRun(t, workspaceBackgroundAgentsSetCmd, []string{"dm_title"}, nil); err == nil {
		t.Fatal("set with neither --agent nor --fallback accepted")
	}
	if _, err := baRun(t, meBackgroundAgentsSetCmd, []string{"routing"}, map[string]string{"agent": baAgentA}); err == nil {
		t.Fatal("routing accepted for me scope")
	}
}

func TestWorkspaceSetMergesAndClearsInReplacedValue(t *testing.T) {
	current := `{"dm_title":{"agent_id":"` + baAgentA + `","fallback_agent_id":"` + baAgentB + `"},"routing":{"agent_id":"` + baAgentC + `","statuses":["todo"]}}`
	reqs, closeSrv := baServer(t, current, `{}`, 200, `{}`)
	defer closeSrv()
	if _, err := baRun(t, workspaceBackgroundAgentsSetCmd, []string{"dm_title"}, map[string]string{"agent": "none"}); err != nil {
		t.Fatal(err)
	}
	w := baWrite(t, reqs)
	if w.Method != http.MethodPut || w.Path != "/api/workspaces/"+baWS+"/background-agents" {
		t.Fatalf("write = %+v", w)
	}
	var got map[string]map[string]any
	_ = json.Unmarshal([]byte(w.Body), &got)
	if _, has := got["dm_title"]["agent_id"]; has || got["dm_title"]["fallback_agent_id"] != baAgentB {
		t.Fatalf("dm_title = %v, want primary cleared and fallback kept", got["dm_title"])
	}
	if got["routing"]["agent_id"] != baAgentC || got["routing"]["statuses"] == nil {
		t.Fatalf("routing not preserved: %v", got["routing"])
	}
}

func TestMachineWriteRefusalPrintsCodeAndFails(t *testing.T) {
	body := `{"error":"background agents can only be changed by a human owner or admin","code":"background_agents_human_only"}`
	for _, cmd := range []*cobra.Command{meBackgroundAgentsSetCmd, workspaceBackgroundAgentsSetCmd} {
		_, closeSrv := baServer(t, `{}`, `{}`, http.StatusForbidden, body)
		_, err := baRun(t, cmd, []string{"dm_title"}, map[string]string{"agent": baAgentA})
		closeSrv()
		if err == nil {
			t.Fatal("403 write returned nil error")
		}
		if msg := cli.FormatError(err, false); !strings.Contains(msg, "background_agents_human_only") {
			t.Fatalf("message %q lacks the 403 code", msg)
		}
		if cli.ExitCodeFor(err) != cli.ExitAuth {
			t.Fatalf("exit code = %d, want %d", cli.ExitCodeFor(err), cli.ExitAuth)
		}
	}
}

func TestMeListJSONShowsChainAndSource(t *testing.T) {
	ws := `{"dm_title":{"agent_id":"` + baAgentA + `"},"quick_actions":{"agent_id":"` + baAgentB + `","fallback_agent_id":"` + baAgentC + `"},"routing":{"agent_id":"` + baAgentC + `"}}`
	mine := `{"dm_title":{"agent_id":"` + baAgentC + `"}}`
	_, closeSrv := baServer(t, ws, mine, 200, `{}`)
	defer closeSrv()
	out, err := baRun(t, meBackgroundAgentsListCmd, nil, map[string]string{"output": "json"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Jobs map[string]struct {
			AgentID         string `json:"agent_id"`
			FallbackAgentID string `json:"fallback_agent_id"`
			Source          string `json:"source"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if j := got.Jobs["dm_title"]; j.Source != "user" || j.AgentID != baAgentC {
		t.Fatalf("dm_title = %+v", j)
	}
	if j := got.Jobs["quick_actions"]; j.Source != "workspace" || j.AgentID != baAgentB || j.FallbackAgentID != baAgentC {
		t.Fatalf("quick_actions = %+v", j)
	}
	if j := got.Jobs["compact"]; j.Source != "off" || j.AgentID != "" {
		t.Fatalf("compact = %+v", j)
	}
	if _, has := got.Jobs["routing"]; has {
		t.Fatal("routing is workspace-only and must not appear for me")
	}
}

func TestWorkspaceListJSONShowsAllJobs(t *testing.T) {
	_, closeSrv := baServer(t, `{"routing":{"agent_id":"`+baAgentC+`"}}`, `{}`, 200, `{}`)
	defer closeSrv()
	out, err := baRun(t, workspaceBackgroundAgentsListCmd, nil, map[string]string{"output": "json"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Jobs map[string]struct{ AgentID, Source string } `json:"jobs"`
	}
	_ = json.Unmarshal([]byte(out), &got)
	if len(got.Jobs) != 4 || got.Jobs["routing"].Source != "workspace" || got.Jobs["dm_title"].Source != "off" {
		t.Fatalf("jobs = %+v (%s)", got.Jobs, out)
	}
}
