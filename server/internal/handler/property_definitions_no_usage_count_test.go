package handler

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6886: the property-definition readers on the decision-wait, park,
// workflow-adoption, upstream-check and required-source paths never read
// usage_count. ListIssueProperties computes it with one COUNT(*) over the
// workspace's issues per definition, so a caller that does not need it must
// not send it. A tracer on the pool sees every statement, including the ones
// inside a transaction, so these tests do not depend on how a caller builds
// its Queries.

type sqlRecorder struct {
	mu   sync.Mutex
	sqls []string
}

func (r *sqlRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = append(r.sqls, data.SQL)
	return ctx
}

func (r *sqlRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// assertDefinitionReadsSkipUsageCount fails when no statement read
// issue_property (the test would prove nothing) or one computed usage_count.
func (r *sqlRecorder) assertDefinitionReadsSkipUsageCount(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	read := false
	for _, sql := range r.sqls {
		if strings.Contains(sql, "FROM issue_property") {
			read = true
		}
		if strings.Contains(sql, "usage_count") {
			t.Errorf("statement computes usage_count the caller never reads:\n%s", sql)
		}
	}
	if !read {
		t.Fatal("no statement read issue_property; the path under test did not run")
	}
}

func tracedHandler(t *testing.T) (*Handler, *sqlRecorder) {
	t.Helper()
	rec := &sqlRecorder{}
	cfg := testPool.Config()
	cfg.ConnConfig.Tracer = rec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := New(db.New(pool), pool, realtime.NewHub(), events.New(), service.NewEmailService(), nil, nil, analytics.NoopClient{}, Config{})
	return h, rec
}

func TestDecisionWaitGuardPassSkipsUsageCount(t *testing.T) {
	h, rec := tracedHandler(t)
	property := dbfx.Insert(t, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Readiness", "type": "select", "config": `{"options":[{"id":"gate","name":"gated-human"}]}`})
	issue := dbfx.Issue(t, "gated human wait", testutil.Cols{"status": "todo"})
	dbfx.Exec(t, `UPDATE issue SET properties = jsonb_build_object($1::text, 'gate'),
		metadata = jsonb_build_object('decision_wait_started_at', to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) WHERE id = $2`, property, issue)

	if err := h.checkDecisionWaits(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.assertDefinitionReadsSkipUsageCount(t)
}

func TestPropertyDefinitionReadersSkipUsageCount(t *testing.T) {
	ctx := context.Background()
	wsID := parseUUID(testWorkspaceID)
	dbfx.Insert(t, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Readiness", "type": "select", "config": `{"options":[{"id":"gate","name":"gated-human"}]}`})
	setup := dbfx.Insert(t, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": setupChangePropertyName, "type": "select", "config": `{"options":[{"id":"yes","name":"yes"}]}`})
	upstream := dbfx.Insert(t, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": upstreamCheckPropertyName, "type": "text"})
	source := dbfx.Insert(t, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Required source", "type": "url"})

	cases := []struct {
		name string
		run  func(t *testing.T, h *Handler) error
	}{
		{"gatedHumanReadiness", func(t *testing.T, h *Handler) error {
			_, _, err := gatedHumanReadiness(ctx, h.Queries, db.Issue{WorkspaceID: wsID})
			return err
		}},
		{"setupChangeValue", func(t *testing.T, h *Handler) error {
			_, err := setupChangeValue(ctx, h.Queries, db.Issue{WorkspaceID: wsID, Properties: []byte(`{"` + setup + `":"yes"}`)})
			return err
		}},
		{"upstreamCheckValue", func(t *testing.T, h *Handler) error {
			_, err := upstreamCheckValue(ctx, h.Queries, wsID, []byte(`{"`+upstream+`":"x"}`))
			return err
		}},
		{"declaredRequiredSource", func(t *testing.T, h *Handler) error {
			got, err := h.declaredRequiredSource(ctx, db.Issue{WorkspaceID: wsID, Properties: []byte(`{"` + source + `":"https://example.test/plan"}`)})
			if err == nil && got != "https://example.test/plan" {
				t.Errorf("declared source = %q", got)
			}
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, rec := tracedHandler(t)
			if err := tc.run(t, h); err != nil {
				t.Fatal(err)
			}
			rec.assertDefinitionReadsSkipUsageCount(t)
		})
	}
}

// Archiving a Required source property stops new values; it does not retract
// an obligation already recorded on an issue.
func TestDeclaredRequiredSourceCountsArchivedDefinition(t *testing.T) {
	source := dbfx.Insert(t, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Required source", "type": "url"})
	dbfx.Exec(t, `UPDATE issue_property SET archived_at = clock_timestamp() WHERE id = $1`, source)
	h, _ := tracedHandler(t)
	got, err := h.declaredRequiredSource(context.Background(), db.Issue{WorkspaceID: parseUUID(testWorkspaceID), Properties: []byte(`{"` + source + `":"https://example.test/plan"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.test/plan" {
		t.Fatalf("archived Required source definition was dropped: got %q", got)
	}
}
