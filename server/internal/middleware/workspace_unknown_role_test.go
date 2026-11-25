package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A member row whose role this build does not know grants no membership
// (CHE-9701). Upstream's 699_member_guest_role widens member's role CHECK to
// admit 'guest'; a pre-699 image rolled back onto that data must not read the
// new role as a full member. The test widens the CHECK the same way 699 does,
// inside a transaction that is rolled back, so no other test sees it.
func TestUnknownMemberRoleGrantsNoMembership(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `ALTER TABLE member
		DROP CONSTRAINT IF EXISTS member_role_check,
		ADD CONSTRAINT member_role_check
		CHECK (role IN ('owner', 'admin', 'member', 'guest')) NOT VALID`); err != nil {
		t.Fatalf("widen member role check: %v", err)
	}

	roles := []string{"owner", "admin", "member", "guest"}
	userIDs := map[string]pgtype.UUID{}
	workspaceIDs := map[string]pgtype.UUID{}
	for _, role := range roles {
		var userID, workspaceID pgtype.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
			"unknown-role-"+role, "unknown-role-"+role+"@che9701.test",
		).Scan(&userID); err != nil {
			t.Fatalf("insert user %s: %v", role, err)
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ($1, $2, '', $3) RETURNING id`,
			"Unknown Role "+role, "che9701-"+role, "UR"+role[:1],
		).Scan(&workspaceID); err != nil {
			t.Fatalf("insert workspace %s: %v", role, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
			workspaceID, userID, role,
		); err != nil {
			t.Fatalf("insert member %s: %v", role, err)
		}
		userIDs[role], workspaceIDs[role] = userID, workspaceID
	}
	queries := db.New(tx)

	t.Run("RequireWorkspaceMember route", func(t *testing.T) {
		route := RequireWorkspaceMember(queries)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		for _, role := range roles {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-User-ID", uuidText(userIDs[role]))
			req.Header.Set("X-Workspace-ID", uuidText(workspaceIDs[role]))
			rec := httptest.NewRecorder()
			route.ServeHTTP(rec, req)
			if role == "guest" {
				if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
					t.Errorf("role guest: status %d, want 403 or 404", rec.Code)
				}
				continue
			}
			if rec.Code != http.StatusOK {
				t.Errorf("role %s: status %d, want 200", role, rec.Code)
			}
		}
	})

	t.Run("workspace list", func(t *testing.T) {
		for _, role := range roles {
			rows, err := queries.ListWorkspaces(ctx, userIDs[role])
			if err != nil {
				t.Fatalf("ListWorkspaces %s: %v", role, err)
			}
			if want := listedWant(role); len(rows) != want {
				t.Errorf("role %s: ListWorkspaces returned %d rows, want %d", role, len(rows), want)
			}
		}
	})

	t.Run("daemon workspace list", func(t *testing.T) {
		for _, role := range roles {
			rows, err := queries.ListDaemonWorkspaces(ctx, userIDs[role])
			if err != nil {
				t.Fatalf("ListDaemonWorkspaces %s: %v", role, err)
			}
			if want := listedWant(role); len(rows) != want {
				t.Errorf("role %s: ListDaemonWorkspaces returned %d rows, want %d", role, len(rows), want)
			}
		}
	})

	t.Run("realtime membership", func(t *testing.T) {
		var users, workspaces []pgtype.UUID
		for _, role := range roles {
			users = append(users, userIDs[role])
			workspaces = append(workspaces, workspaceIDs[role])
		}
		rows, err := queries.GetRealtimeMemberships(ctx, db.GetRealtimeMembershipsParams{UserIds: users, WorkspaceIds: workspaces})
		if err != nil {
			t.Fatalf("GetRealtimeMemberships: %v", err)
		}
		if len(rows) != 3 {
			t.Errorf("GetRealtimeMemberships returned %d rows, want 3 (owner, admin, member)", len(rows))
		}
		for _, row := range rows {
			if row.UserID == userIDs["guest"] {
				t.Errorf("guest user kept a realtime membership")
			}
		}
	})

	t.Run("inbox delivery and unread dot", func(t *testing.T) {
		for _, role := range roles {
			_, err := queries.CreateInboxItem(ctx, db.CreateInboxItemParams{
				WorkspaceID:   workspaceIDs[role],
				RecipientType: "member",
				RecipientID:   userIDs[role],
				Type:          "new_comment",
				Severity:      "info",
				Title:         "unknown role probe",
				Details:       []byte(`{}`),
			})
			if role == "guest" {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Errorf("guest inbox delivery: err %v, want pgx.ErrNoRows (quiet skip)", err)
				}
				continue
			}
			if err != nil {
				t.Errorf("role %s inbox delivery: %v", role, err)
			}
		}
		// The guest holds a stray unread item written before the rollback.
		if _, err := tx.Exec(ctx,
			`INSERT INTO inbox_item (workspace_id, recipient_type, recipient_id, type, severity, title, details)
			 VALUES ($1, 'member', $2, 'new_comment', 'info', 'stray', '{}')`,
			workspaceIDs["guest"], userIDs["guest"],
		); err != nil {
			t.Fatalf("seed guest inbox item: %v", err)
		}
		rows, err := queries.CountUnreadInboxByWorkspace(ctx, userIDs["guest"])
		if err != nil {
			t.Fatalf("CountUnreadInboxByWorkspace: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("guest unread dot lit for %d workspaces, want 0", len(rows))
		}
	})
}

// listedWant is how many workspace rows a user holding role sees: one for a
// role this build knows, none otherwise.
func listedWant(role string) int {
	if role == "guest" {
		return 0
	}
	return 1
}

func uuidText(id pgtype.UUID) string {
	v, _ := id.Value()
	s, _ := v.(string)
	return s
}
