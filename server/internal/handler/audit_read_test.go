package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5644: GET /api/workspaces/{id}/audit-events. Rows are seeded straight
// into workspace_audit_event for two real workspaces.

type auditReadEvent struct {
	ID                string          `json:"id"`
	At                time.Time       `json:"at"`
	ActorType         string          `json:"actor_type"`
	ActorID           *string         `json:"actor_id"`
	ActorName         *string         `json:"actor_name"`
	TaskID            *string         `json:"task_id"`
	Kind              string          `json:"kind"`
	TargetType        string          `json:"target_type"`
	TargetID          *string         `json:"target_id"`
	TargetName        *string         `json:"target_name"`
	Before            json.RawMessage `json:"before"`
	After             json.RawMessage `json:"after"`
	ExternalRequested []string        `json:"external_requested"`
}

type auditReadPage struct {
	Events     []auditReadEvent `json:"events"`
	NextCursor *string          `json:"next_cursor"`
}

// auditReadFixture is workspace A (admin, member) and workspace B (its own admin).
type auditReadFixture struct {
	wsA, wsB                        string
	adminA, memberA, adminB, nobody string
}

func newAuditReadFixture(t *testing.T) auditReadFixture {
	t.Helper()
	n := time.Now().UnixNano()
	f := auditReadFixture{}
	f.wsA = dbfx.Workspace(t, "Audit Read A", fmt.Sprintf("audit-read-a-%d", n))
	f.wsB = dbfx.Workspace(t, "Audit Read B", fmt.Sprintf("audit-read-b-%d", n))
	mk := func(label, ws, role string) string {
		u := dbfx.User(t, "Audit "+label, fmt.Sprintf("audit-read-%s-%d@multica.test", label, n))
		if ws != "" {
			dbfx.Member(t, ws, u, role)
		}
		return u
	}
	f.adminA, f.memberA = mk("adminA", f.wsA, "admin"), mk("memberA", f.wsA, "member")
	f.adminB, f.nobody = mk("adminB", f.wsB, "owner"), mk("nobody", "", "")
	// Registered after the workspaces, so it runs first (the table is append-only).
	for _, ws := range []string{f.wsA, f.wsB} {
		ws := ws
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `SELECT audit_workspace_delete($1)`, ws)
		})
	}
	return f
}

type seedRow struct {
	ws, kind, targetType  string
	at                    time.Time
	id, targetID, actorID string
	actorType             string
}

func seedAudit(t *testing.T, r seedRow) string {
	t.Helper()
	if r.id == "" {
		r.id = uuid.NewString()
	}
	if r.actorType == "" {
		r.actorType = "member"
	}
	if r.targetType == "" {
		r.targetType = "workspace"
	}
	var actor, target any
	if r.actorID != "" {
		actor = r.actorID
	}
	if r.targetID != "" {
		target = r.targetID
	}
	if err := auditExec(t, `
		INSERT INTO workspace_audit_event (id, workspace_id, at, actor_type, actor_id, kind, target_type, target_id, "after")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, '{"name":"changed"}')`,
		r.id, r.ws, r.at, r.actorType, actor, r.kind, r.targetType, target); err != nil {
		t.Fatalf("seed audit row: %v", err)
	}
	return r.id
}

func auditGet(t *testing.T, ws, userID string, query url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("GET", "/api/workspaces/"+ws+"/audit-events?"+query.Encode(), nil)
	req.Header.Set("X-User-ID", userID)
	req.Header.Set("X-Workspace-ID", ws)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = withURLParam(req, "id", ws)
	w := httptest.NewRecorder()
	testHandler.ListWorkspaceAuditEvents(w, req)
	return w
}

func auditPageOK(t *testing.T, ws, userID string, query url.Values) auditReadPage {
	t.Helper()
	w := auditGet(t, ws, userID, query, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var page auditReadPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v (%q)", err, w.Body.String())
	}
	return page
}

func eventIDs(p auditReadPage) []string {
	out := make([]string, len(p.Events))
	for i, e := range p.Events {
		out[i] = e.ID
	}
	return out
}

func TestAuditRead_Access(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newAuditReadFixture(t)
	seedAudit(t, seedRow{ws: f.wsA, kind: "workspace.update", at: time.Now()})

	cases := []struct {
		name   string
		userID string
		ws     string
		hdr    map[string]string
		want   int
	}{
		{"admin", f.adminA, f.wsA, nil, http.StatusOK},
		{"member", f.memberA, f.wsA, nil, http.StatusForbidden},
		{"non-member", f.nobody, f.wsA, nil, http.StatusNotFound},
		{"other workspace admin", f.adminB, f.wsA, nil, http.StatusNotFound},
		{"agent task token", f.adminA, f.wsA, map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": uuid.NewString()}, http.StatusForbidden},
		{"cloud pat", f.adminA, f.wsA, map[string]string{"X-Actor-Source": "cloud_pat"}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := auditGet(t, c.ws, c.userID, nil, c.hdr).Code; got != c.want {
				t.Fatalf("status = %d, want %d", got, c.want)
			}
		})
	}
}

func TestAuditRead_WorkspaceIsolation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newAuditReadFixture(t)
	a := seedAudit(t, seedRow{ws: f.wsA, kind: "workspace.update", at: time.Now()})
	seedAudit(t, seedRow{ws: f.wsB, kind: "workspace.update", at: time.Now()})
	seedAudit(t, seedRow{ws: f.wsB, kind: "agent.update", at: time.Now()})

	got := eventIDs(auditPageOK(t, f.wsA, f.adminA, nil))
	if len(got) != 1 || got[0] != a {
		t.Fatalf("workspace A admin saw %v, want only [%s]", got, a)
	}
}

func TestAuditRead_Filters(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newAuditReadFixture(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	target1, target2 := uuid.NewString(), uuid.NewString()
	actor1 := f.adminA
	rows := map[string]string{
		"agentUpdate":  seedAudit(t, seedRow{ws: f.wsA, kind: "agent.update", targetType: "agent", targetID: target1, actorID: actor1, at: base}),
		"agentEnv":     seedAudit(t, seedRow{ws: f.wsA, kind: "agent.env.update", targetType: "agent", targetID: target2, at: base.Add(1 * time.Minute)}),
		"wsUpdate":     seedAudit(t, seedRow{ws: f.wsA, kind: "workspace.update", at: base.Add(2 * time.Minute)}),
		"percentKind":  seedAudit(t, seedRow{ws: f.wsA, kind: "x%y_z", at: base.Add(3 * time.Minute)}),
		"otherPercent": seedAudit(t, seedRow{ws: f.wsA, kind: "xAyBz", at: base.Add(4 * time.Minute)}),
	}
	ts := func(d time.Duration) string { return base.Add(d).UTC().Format(time.RFC3339) }

	cases := []struct {
		name  string
		query url.Values
		want  []string // newest first
	}{
		{"no filter", nil, []string{"otherPercent", "percentKind", "wsUpdate", "agentEnv", "agentUpdate"}},
		{"kind prefix", url.Values{"kind": {"agent."}}, []string{"agentEnv", "agentUpdate"}},
		{"kind exact", url.Values{"kind": {"agent.update"}}, []string{"agentUpdate"}},
		{"kind percent is literal", url.Values{"kind": {"x%"}}, []string{"percentKind"}},
		{"kind underscore is literal", url.Values{"kind": {"x%y_"}}, []string{"percentKind"}},
		{"kind no match", url.Values{"kind": {"nope"}}, nil},
		{"target_id", url.Values{"target_id": {target2}}, []string{"agentEnv"}},
		{"actor_id", url.Values{"actor_id": {actor1}}, []string{"agentUpdate"}},
		{"since inclusive", url.Values{"since": {ts(2 * time.Minute)}}, []string{"otherPercent", "percentKind", "wsUpdate"}},
		{"until exclusive", url.Values{"until": {ts(2 * time.Minute)}}, []string{"agentEnv", "agentUpdate"}},
		{"since and until", url.Values{"since": {ts(1 * time.Minute)}, "until": {ts(3 * time.Minute)}}, []string{"wsUpdate", "agentEnv"}},
		{"combined", url.Values{"kind": {"agent."}, "target_id": {target1}}, []string{"agentUpdate"}},
		{"limit", url.Values{"limit": {"2"}}, []string{"otherPercent", "percentKind"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := eventIDs(auditPageOK(t, f.wsA, f.adminA, c.query))
			var want []string
			for _, k := range c.want {
				want = append(want, rows[k])
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("got %d rows %v, want %d rows %v", len(got), got, len(want), want)
			}
		})
	}
}

func TestAuditRead_BadInput(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newAuditReadFixture(t)
	for name, q := range map[string]url.Values{
		"target_id uuid": {"target_id": {"nope"}},
		"actor_id uuid":  {"actor_id": {"nope"}},
		"since time":     {"since": {"yesterday"}},
		"until time":     {"until": {"2026-13-45"}},
		"cursor":         {"cursor": {"!!not-a-cursor"}},
		"limit text":     {"limit": {"many"}},
		"limit zero":     {"limit": {"0"}},
		"limit negative": {"limit": {"-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := auditGet(t, f.wsA, f.adminA, q, nil).Code; got != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", got)
			}
		})
	}
	t.Run("bad workspace id", func(t *testing.T) {
		if got := auditGet(t, "not-a-uuid", f.adminA, nil, nil).Code; got != http.StatusBadRequest && got != http.StatusNotFound {
			t.Fatalf("status = %d, want 400 or 404", got)
		}
	})
}

func TestAuditRead_LimitIsCapped(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newAuditReadFixture(t)
	if err := auditExec(t, `
		INSERT INTO workspace_audit_event (id, workspace_id, at, actor_type, kind, target_type)
		SELECT gen_random_uuid(), $1, now() - make_interval(secs => g), 'member', 'workspace.update', 'workspace'
		FROM generate_series(1, 230) g`, f.wsA); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n := len(auditPageOK(t, f.wsA, f.adminA, nil).Events); n != 50 {
		t.Fatalf("default page = %d rows, want 50", n)
	}
	if n := len(auditPageOK(t, f.wsA, f.adminA, url.Values{"limit": {"1000"}}).Events); n != 200 {
		t.Fatalf("limit=1000 page = %d rows, want 200 (max)", n)
	}
}

// Rows that share one `at` must neither repeat nor vanish across pages.
func TestAuditRead_KeysetPaginationWithEqualAt(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newAuditReadFixture(t)
	tie := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	want := map[string]bool{}
	for i := 0; i < 7; i++ {
		want[seedAudit(t, seedRow{ws: f.wsA, kind: "workspace.update", at: tie})] = true
	}
	for i := 0; i < 4; i++ {
		want[seedAudit(t, seedRow{ws: f.wsA, kind: "workspace.update", at: tie.Add(time.Duration(i-2) * time.Minute)})] = true
	}

	seen := map[string]bool{}
	var order []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
		q := url.Values{"limit": {"3"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		page := auditPageOK(t, f.wsA, f.adminA, q)
		for _, e := range page.Events {
			if seen[e.ID] {
				t.Fatalf("row %s repeated across pages", e.ID)
			}
			seen[e.ID] = true
			order = append(order, e.ID)
		}
		if page.NextCursor == nil {
			break
		}
		if len(page.Events) == 0 {
			t.Fatal("empty page with a next_cursor")
		}
		cursor = *page.NextCursor
	}
	if len(seen) != len(want) {
		t.Fatalf("saw %d rows across pages, want %d", len(seen), len(want))
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("row %s missing from pages", id)
		}
	}
	// Last page of exactly `limit` rows must not hand out a cursor.
	if page := auditPageOK(t, f.wsA, f.adminA, url.Values{"limit": {"11"}}); page.NextCursor != nil {
		t.Errorf("next_cursor = %q on the final page, want null", *page.NextCursor)
	}
}

func TestAuditRead_RowFieldsAndNames(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newAuditReadFixture(t)
	agentID := dbfx.Agent(t, "Audit Read Agent", handlerTestRuntimeID(t), testutil.Cols{"workspace_id": f.wsA, "owner_id": f.adminA})
	taskID := uuid.NewString()
	at := time.Now().Add(-time.Minute).Truncate(time.Microsecond)

	id := uuid.NewString()
	if err := auditExec(t, `
		INSERT INTO workspace_audit_event (id, workspace_id, at, actor_type, actor_id, task_id, actor_source, kind, target_type, target_id, "before", "after", external_requested)
		VALUES ($1, $2, $3, 'agent', $4, $5, 'task_token', 'agent.update', 'agent', $4, '{"name":"old"}', '{"instructions":"changed"}', ARRAY['daemon_update_request'])`,
		id, f.wsA, at, agentID, taskID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	memberRow := seedAudit(t, seedRow{ws: f.wsA, kind: "workspace.update", actorID: f.adminA, at: at.Add(-time.Minute)})
	goneRow := seedAudit(t, seedRow{ws: f.wsA, kind: "agent.update", targetType: "agent", targetID: uuid.NewString(), at: at.Add(-2 * time.Minute)})

	byID := map[string]auditReadEvent{}
	for _, e := range auditPageOK(t, f.wsA, f.adminA, nil).Events {
		byID[e.ID] = e
	}

	ev, ok := byID[id]
	if !ok {
		t.Fatalf("seeded agent row missing; got %d rows", len(byID))
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	if ev.ActorType != "agent" || str(ev.ActorID) != agentID || str(ev.TaskID) != taskID {
		t.Errorf("actor = %s/%s task %s, want agent/%s task %s", ev.ActorType, str(ev.ActorID), str(ev.TaskID), agentID, taskID)
	}
	if str(ev.ActorName) != "Audit Read Agent" || str(ev.TargetName) != "Audit Read Agent" {
		t.Errorf("names = actor %q target %q, want both %q", str(ev.ActorName), str(ev.TargetName), "Audit Read Agent")
	}
	if ev.Kind != "agent.update" || ev.TargetType != "agent" || str(ev.TargetID) != agentID {
		t.Errorf("kind/target = %s %s %s", ev.Kind, ev.TargetType, str(ev.TargetID))
	}
	if string(ev.Before) != `{"name": "old"}` && string(ev.Before) != `{"name":"old"}` {
		t.Errorf("before = %s", ev.Before)
	}
	var after map[string]string
	if err := json.Unmarshal(ev.After, &after); err != nil || after["instructions"] != "changed" {
		t.Errorf("after = %s, want the stored changed marker", ev.After)
	}
	if len(ev.ExternalRequested) != 1 || ev.ExternalRequested[0] != "daemon_update_request" {
		t.Errorf("external_requested = %v", ev.ExternalRequested)
	}
	if !ev.At.Equal(at) {
		t.Errorf("at = %s, want %s", ev.At, at)
	}

	if m := byID[memberRow]; str(m.ActorName) != "Audit adminA" || m.TaskID != nil {
		t.Errorf("member row actor name %q task %v, want %q and no task", str(m.ActorName), m.TaskID, "Audit adminA")
	}
	gone := byID[goneRow]
	if gone.TargetName != nil {
		t.Errorf("deleted target name = %q, want null", *gone.TargetName)
	}
	if gone.ActorName != nil || gone.ActorID != nil {
		t.Errorf("row without actor id shows actor %v/%v, want null", gone.ActorID, gone.ActorName)
	}
	if gone.ExternalRequested == nil {
		t.Errorf("external_requested = null, want []")
	}
}
