package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	auWS     = "00000000-0000-0000-0000-0000000000ws"
	auAgentA = "11111111-1111-1111-1111-111111111111"
	auAgentB = "22222222-2222-2222-2222-222222222222"
	auAgentC = "33333333-3333-3333-3333-333333333333"
	auActor  = "44444444-4444-4444-4444-444444444444"
)

const auEvent = `{"id":"e1","at":"2026-11-14T01:00:00Z","actor_type":"member","actor_id":"` + auActor + `","actor_name":"Cheese","task_id":null,"kind":"agent.update","target_type":"agent","target_id":"` + auAgentC + `","target_name":"Luna","before":{"instructions":"changed"},"after":null,"external_requested":[]}`

// auServer serves the agent list and audit pages in order, recording queries.
func auServer(t *testing.T, pages ...string) (*[]url.Values, *int) {
	t.Helper()
	var mu sync.Mutex
	var queries []url.Values
	auditHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/agents":
			w.Write([]byte(`[{"id":"` + auAgentA + `","name":"Terra Haiku"},{"id":"` + auAgentB + `","name":"Terra Sonnet"},{"id":"` + auAgentC + `","name":"Luna"}]`))
		case "/api/workspaces/" + auWS + "/audit-events":
			queries = append(queries, r.URL.Query())
			i := min(auditHits, len(pages)-1)
			auditHits++
			w.Write([]byte(pages[i]))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", auWS)
	return &queries, &auditHits
}

func auRun(t *testing.T, flags map[string]string) (string, error) {
	t.Helper()
	for _, name := range []string{"kind", "target", "actor", "since", "until", "limit", "all", "output"} {
		if f := auditListCmd.Flags().Lookup(name); f != nil {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}
	for k, v := range flags {
		if err := auditListCmd.Flags().Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	return captureStdout(t, func() error { return auditListCmd.RunE(auditListCmd, nil) })
}

func TestAuditListFlagsBuildQuery(t *testing.T) {
	queries, _ := auServer(t, `{"events":[],"next_cursor":null}`)
	_, err := auRun(t, map[string]string{"kind": "agent.", "actor": auActor, "target": auAgentB, "limit": "25", "until": "2026-11-14T02:00:00Z", "output": "json"})
	if err != nil {
		t.Fatal(err)
	}
	q := (*queries)[0]
	want := map[string]string{"kind": "agent.", "actor_id": auActor, "target_id": auAgentB, "limit": "25", "until": "2026-11-14T02:00:00Z"}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("query %s = %q, want %q (query %v)", k, q.Get(k), v, q)
		}
	}
}

func TestAuditListSinceDurationBecomesRFC3339(t *testing.T) {
	for _, tc := range []struct {
		in  string
		ago time.Duration
	}{{"2h", 2 * time.Hour}, {"1d", 24 * time.Hour}, {"90m", 90 * time.Minute}} {
		queries, _ := auServer(t, `{"events":[],"next_cursor":null}`)
		before := time.Now()
		if _, err := auRun(t, map[string]string{"since": tc.in, "output": "json"}); err != nil {
			t.Fatal(err)
		}
		got, err := time.Parse(time.RFC3339Nano, (*queries)[0].Get("since"))
		if err != nil {
			t.Fatalf("--since %s sent %q, want RFC 3339: %v", tc.in, (*queries)[0].Get("since"), err)
		}
		if d := before.Add(-tc.ago).Sub(got); d < -5*time.Second || d > 5*time.Second {
			t.Errorf("--since %s = %s, want about %s ago", tc.in, got, tc.ago)
		}
	}
}

func TestAuditListSinceKeepsRFC3339AndRejectsGarbage(t *testing.T) {
	queries, _ := auServer(t, `{"events":[],"next_cursor":null}`)
	if _, err := auRun(t, map[string]string{"since": "2026-11-14T00:00:00Z", "output": "json"}); err != nil {
		t.Fatal(err)
	}
	if got := (*queries)[0].Get("since"); got != "2026-11-14T00:00:00Z" {
		t.Errorf("since = %q", got)
	}
	_, hits := auServer(t, `{"events":[],"next_cursor":null}`)
	if _, err := auRun(t, map[string]string{"since": "yesterday-ish"}); err == nil {
		t.Error("bad --since accepted")
	}
	if *hits != 0 {
		t.Error("request sent despite a bad --since")
	}
}

func TestAuditListTargetResolvesName(t *testing.T) {
	queries, _ := auServer(t, `{"events":[],"next_cursor":null}`)
	if _, err := auRun(t, map[string]string{"target": "luna", "output": "json"}); err != nil {
		t.Fatal(err)
	}
	if got := (*queries)[0].Get("target_id"); got != auAgentC {
		t.Errorf("target_id = %q, want %q", got, auAgentC)
	}
}

func TestAuditListAmbiguousTargetListsCandidatesAndSendsNothing(t *testing.T) {
	_, hits := auServer(t, `{"events":[],"next_cursor":null}`)
	_, err := auRun(t, map[string]string{"target": "terra"})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "Terra Haiku") || !strings.Contains(err.Error(), "Terra Sonnet") {
		t.Fatalf("err = %v, want ambiguous with both candidates", err)
	}
	if *hits != 0 {
		t.Error("audit request sent despite ambiguous target")
	}
}

func TestAuditListJSONShape(t *testing.T) {
	auServer(t, `{"events":[`+auEvent+`],"next_cursor":null}`)
	out, err := auRun(t, map[string]string{"output": "json"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(got.Events) != 1 || got.Events[0]["kind"] != "agent.update" || got.Events[0]["actor_name"] != "Cheese" {
		t.Fatalf("events = %v", got.Events)
	}
	if before, _ := got.Events[0]["before"].(map[string]any); before["instructions"] != "changed" {
		t.Errorf("before lost: %v", got.Events[0]["before"])
	}
}

func TestAuditListTablePrintsRow(t *testing.T) {
	auServer(t, `{"events":[`+auEvent+`],"next_cursor":null}`)
	out, err := auRun(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AT", "ACTOR", "KIND", "TARGET", "agent.update", "Cheese", "Luna"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

func TestAuditListAllFollowsCursor(t *testing.T) {
	e2 := strings.Replace(auEvent, `"id":"e1"`, `"id":"e2"`, 1)
	queries, hits := auServer(t,
		`{"events":[`+auEvent+`],"next_cursor":"c1"}`,
		`{"events":[`+e2+`],"next_cursor":null}`)
	out, err := auRun(t, map[string]string{"all": "true", "output": "json"})
	if err != nil {
		t.Fatal(err)
	}
	if *hits != 2 || (*queries)[1].Get("cursor") != "c1" || (*queries)[0].Get("cursor") != "" {
		t.Fatalf("hits=%d queries=%v", *hits, *queries)
	}
	var got struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Events) != 2 {
		t.Fatalf("events merged = %v err %v\n%s", got.Events, err, out)
	}
}

func TestAuditListWithoutAllStopsAfterOnePageAndShowsCursor(t *testing.T) {
	_, hits := auServer(t, `{"events":[`+auEvent+`],"next_cursor":"c1"}`, `{"events":[],"next_cursor":null}`)
	out, err := auRun(t, map[string]string{"output": "json"})
	if err != nil {
		t.Fatal(err)
	}
	if *hits != 1 || !strings.Contains(out, `"next_cursor": "c1"`) {
		t.Fatalf("hits=%d out=%s", *hits, out)
	}
}
