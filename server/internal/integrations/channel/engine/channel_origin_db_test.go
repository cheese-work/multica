package engine

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestChannelOriginBindingLifecycle(t *testing.T) {
	for _, generation := range []bool{false, true} {
		name := "binding"
		if generation {
			name = "generation"
		}
		t.Run(name, func(t *testing.T) {
			pool := sessionPersistenceTestDB(t)
			ctx := context.Background()
			fixture := dbfx.New(pool, "", "")
			fixture.UserID = fixture.User(t, "Channel origin", "channel-origin-"+uuid.NewString()+"@multica.test")
			fixture.WorkspaceID = fixture.Workspace(t, "Channel origin", "channel-origin-"+uuid.NewString())
			runtimeID := fixture.Runtime(t, "Channel origin")
			agentID := fixture.Agent(t, "Channel origin", runtimeID)
			sessionID := util.MustParseUUID(fixture.ChatSession(t, agentID))
			webSessionID := util.MustParseUUID(fixture.ChatSession(t, agentID))
			installationID := util.MustParseUUID(fixture.Insert(t, "channel_installation", dbfx.Cols{
				"workspace_id": fixture.WorkspaceID, "agent_id": agentID, "channel_type": "lark",
				"config": dbfx.Raw("'{}'::jsonb"), "status": "active", "installer_user_id": fixture.UserID,
			}))
			fixture.Cleanup(t, `DELETE FROM channel_chat_session_binding WHERE installation_id = $1`, installationID)
			fixture.Cleanup(t, `DELETE FROM channel_chat_context_generation WHERE chat_session_id = $1`, sessionID)
			queries := db.New(pool)
			bindingKey := "origin-" + uuid.NewString()
			bind := func(queries *db.Queries, target pgtype.UUID) error {
				if generation {
					_, err := queries.CreateChannelChatSessionBindingGeneration(ctx, db.CreateChannelChatSessionBindingGenerationParams{
						ChatSessionID: target, InstallationID: installationID, ChannelType: "lark",
						ChannelChatID: bindingKey, ChatType: "p2p", Config: []byte("{}"), RouteRevision: 1,
					})
					return err
				}
				_, err := queries.CreateChannelChatSessionBinding(ctx, db.CreateChannelChatSessionBindingParams{
					ChatSessionID: target, InstallationID: installationID, ChannelType: "lark",
					ChannelChatID: bindingKey, ChatType: "p2p", Config: []byte("{}"),
				})
				return err
			}
			origin := func(target pgtype.UUID) pgtype.Timestamptz {
				var timestamp pgtype.Timestamptz
				fixture.QueryRow(t, `SELECT channel_origin_at FROM chat_session WHERE id = $1`, target).Scan(&timestamp)
				return timestamp
			}
			if origin(sessionID).Valid || origin(webSessionID).Valid {
				t.Fatal("new web sessions must have no channel origin")
			}
			transaction, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer transaction.Rollback(ctx)
			if err := bind(queries.WithTx(transaction), sessionID); err != nil {
				t.Fatal(err)
			}
			if err := transaction.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if origin(sessionID).Valid {
				t.Fatal("rolled-back binding must not establish origin")
			}
			if err := bind(queries, sessionID); err != nil {
				t.Fatal(err)
			}
			first := origin(sessionID)
			if !first.Valid {
				t.Fatal("first binding must establish channel origin")
			}
			var bindingCreatedAt pgtype.Timestamptz
			fixture.QueryRow(t, `SELECT created_at FROM channel_chat_session_binding WHERE chat_session_id = $1`, sessionID).Scan(&bindingCreatedAt)
			if !first.Time.Equal(bindingCreatedAt.Time) {
				t.Fatalf("origin=%v, want first binding creation=%v", first, bindingCreatedAt)
			}
			if err := bind(queries, webSessionID); err == nil {
				t.Fatal("duplicate channel route must fail")
			}
			if origin(webSessionID).Valid {
				t.Fatal("failed binding must not mark a web session as channel-origin")
			}
			if err := queries.DeleteChannelChatSessionBindingBySession(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			if got := origin(sessionID); !got.Valid || !got.Time.Equal(first.Time) {
				t.Fatalf("unbind changed first origin: got=%v, first=%v", got, first)
			}
			if err := bind(queries, sessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := queries.SetChatSessionArchived(ctx, db.SetChatSessionArchivedParams{ID: sessionID, Archived: true}); err != nil {
				t.Fatal(err)
			}
			if err := queries.DeleteChannelChatSessionBindingBySession(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := queries.SetChatSessionArchived(ctx, db.SetChatSessionArchivedParams{ID: sessionID}); err != nil {
				t.Fatal(err)
			}
			if got := origin(sessionID); !got.Valid || !got.Time.Equal(first.Time) {
				t.Fatalf("rebind/archive/unarchive changed first origin: got=%v, first=%v", got, first)
			}
			if err := bind(queries, sessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := queries.ClearChannelInstallationBotScopedRows(ctx, installationID); err != nil {
				t.Fatal(err)
			}
			fixture.Exec(t, `DELETE FROM channel_installation WHERE id = $1`, installationID)
			if count := fixture.Count(t, `SELECT count(*) FROM channel_chat_session_binding WHERE chat_session_id = $1`, sessionID); count != 0 {
				t.Fatalf("installation removal left %d bindings", count)
			}
			if got := origin(sessionID); !got.Valid || !got.Time.Equal(first.Time) {
				t.Fatalf("installation removal changed first origin: got=%v, first=%v", got, first)
			}
			if origin(webSessionID).Valid {
				t.Fatal("unrelated web session acquired channel origin")
			}
		})
	}
}
