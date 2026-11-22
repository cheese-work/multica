package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/queueorder"
)

// CHE-8761 (S2 of CHE-8749): queue_order and queue_aging_minutes. Owner and
// admin write them; a refused value names the allowed values; each change is
// audited as actor, old, new, time.

var queueOrderRefused = []struct {
	name, key string
	value     any
	want      []string
}{
	{"unknown mode", queueorder.KeyOrder, "fifo", []string{"priority", "project_rank"}},
	{"mode not a string", queueorder.KeyOrder, 7, []string{"priority", "project_rank"}},
	{"mode null", queueorder.KeyOrder, nil, []string{"priority", "project_rank"}},
	{"aging below 5", queueorder.KeyAging, 4, []string{"5", "720"}},
	{"aging above 720", queueorder.KeyAging, 721, []string{"5", "720"}},
	{"aging fraction", queueorder.KeyAging, 30.5, []string{"5", "720"}},
	{"aging string", queueorder.KeyAging, "30", []string{"5", "720"}},
}

func queueOrderAudit(t *testing.T) (rows []struct {
	ActorType, ActorID, Before, After string
	At                                time.Time
}) {
	t.Helper()
	dbrows, err := testPool.Query(context.Background(), `
		SELECT actor_type, COALESCE(actor_id::text, ''), "before"::text, "after"::text, at
		FROM workspace_audit_event
		WHERE workspace_id = $1 AND kind = 'workspace.queue_order'
		ORDER BY at, id`, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	defer dbrows.Close()
	for dbrows.Next() {
		var row struct {
			ActorType, ActorID, Before, After string
			At                                time.Time
		}
		if err := dbrows.Scan(&row.ActorType, &row.ActorID, &row.Before, &row.After, &row.At); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestWorkspaceQueueOrderOwnerFields(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setWorkspaceSettings(t, `{"compact_prompt":"Keep SHAs.","merge_queue_dequeue_routing":"warn"}`)
	mqUpdate(t, map[string]any{queueorder.KeyOrder: "project_rank"}).Want(http.StatusOK)
	mqUpdate(t, map[string]any{queueorder.KeyAging: 45}).Want(http.StatusOK)
	s := mqSettings(t)
	if s[queueorder.KeyOrder] != "project_rank" || s[queueorder.KeyAging] != float64(45) {
		t.Fatalf("values not stored: %v", s)
	}
	if s["compact_prompt"] != "Keep SHAs." || s["merge_queue_dequeue_routing"] != "warn" {
		t.Errorf("a queue field changed another key: %v", s)
	}
	for _, tc := range queueOrderRefused {
		before := mqSettings(t)[tc.key]
		body := mqUpdate(t, map[string]any{tc.key: tc.value}).Want(http.StatusBadRequest).Body.String()
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s: body %q does not name %q", tc.name, body, want)
			}
		}
		if got := mqSettings(t)[tc.key]; mqJSON(t, got) != mqJSON(t, before) {
			t.Errorf("%s: a refused write changed %s to %v", tc.name, tc.key, got)
		}
	}
}

func TestGenericSettingsSaveValidatesQueueOrder(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setWorkspaceSettings(t, `{"compact_prompt":"keep"}`)
	mqUpdate(t, map[string]any{"settings": map[string]any{"compact_prompt": "keep", queueorder.KeyOrder: "project_rank"}}).Want(http.StatusOK)
	if got := mqSettings(t)[queueorder.KeyOrder]; got != "project_rank" {
		t.Fatalf("valid mode not stored: %v", got)
	}
	for _, tc := range queueOrderRefused {
		body := mqUpdate(t, map[string]any{"settings": map[string]any{tc.key: tc.value}}).Want(http.StatusBadRequest).Body.String()
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s: body %q does not name %q", tc.name, body, want)
			}
		}
	}
	if got := mqSettings(t)[queueorder.KeyOrder]; got != "project_rank" {
		t.Errorf("a refused save changed the mode: %v", got)
	}
}

func TestQueueOrderWriteNeedsOwnerOrAdmin(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setWorkspaceSettings(t, `{}`)
	t.Cleanup(func() { setRuntimeTestMemberRole(t, testUserID, "owner") })

	auditBefore := len(queueOrderAudit(t))
	setRuntimeTestMemberRole(t, testUserID, "member")
	for _, body := range []map[string]any{
		{queueorder.KeyOrder: "project_rank"},
		{queueorder.KeyAging: 60},
	} {
		mqUpdate(t, body).Want(http.StatusForbidden)
	}
	if s := mqSettings(t); s[queueorder.KeyOrder] != nil || s[queueorder.KeyAging] != nil {
		t.Fatalf("a member wrote a queue key: %v", s)
	}
	if rows := queueOrderAudit(t); len(rows) != auditBefore {
		t.Fatalf("a refused member write left %d new audit rows", len(rows)-auditBefore)
	}

	setRuntimeTestMemberRole(t, testUserID, "admin")
	mqUpdate(t, map[string]any{queueorder.KeyOrder: "project_rank"}).Want(http.StatusOK)
	if got := mqSettings(t)[queueorder.KeyOrder]; got != "project_rank" {
		t.Fatalf("admin write not stored: %v", got)
	}
}

func TestQueueOrderChangeIsAudited(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setWorkspaceSettings(t, `{}`)
	start := queueOrderAudit(t)
	mqUpdate(t, map[string]any{queueorder.KeyOrder: "project_rank"}).Want(http.StatusOK)
	mqUpdate(t, map[string]any{queueorder.KeyOrder: "project_rank"}).Want(http.StatusOK) // no change
	mqUpdate(t, map[string]any{queueorder.KeyAging: 90}).Want(http.StatusOK)

	rows := queueOrderAudit(t)[len(start):]
	if len(rows) != 2 {
		t.Fatalf("got %d audit rows, want 2 (a repeated value writes none): %+v", len(rows), rows)
	}
	first, second := rows[0], rows[1]
	if first.ActorType != "member" || first.ActorID != testUserID {
		t.Errorf("actor = %s/%s, want member/%s", first.ActorType, first.ActorID, testUserID)
	}
	if time.Since(first.At) > time.Minute || first.At.IsZero() {
		t.Errorf("time %v is not recent", first.At)
	}
	read := func(raw string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Old and new are the effective values, so an unset key reads as its default.
	if b := read(first.Before); b[queueorder.KeyOrder] != "priority" {
		t.Errorf("first change old = %v, want queue_order priority", b)
	}
	if a := read(first.After); a[queueorder.KeyOrder] != "project_rank" {
		t.Errorf("first change new = %v, want queue_order project_rank", a)
	}
	if b := read(second.Before); b[queueorder.KeyAging] != float64(30) {
		t.Errorf("second change old = %v, want aging 30", b)
	}
	if a := read(second.After); a[queueorder.KeyAging] != float64(90) {
		t.Errorf("second change new = %v, want aging 90", a)
	}
}
