package handler

import (
	"context"
	"strings"
	"testing"
)

// The snippet window is positioned from the search text. Under an ICU database
// LOWER maps one character to several (U+0130 becomes "i" + U+0307), so a
// position found in the lowercased text points past the same place in the
// original and the excerpt loses the match. The test runs chatSearchSQL against
// session-local copies of its tables so the message column can carry either
// collation without touching shared data.
func TestChatSearchSnippetKeepsMatchAcrossCollations(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	for _, collation := range []string{`"default"`, `"und-x-icu"`} {
		t.Run(strings.Trim(collation, `"`), func(t *testing.T) {
			ctx := context.Background()
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)

			ddl := []string{
				`CREATE TEMP TABLE chat_session (id uuid, workspace_id uuid, creator_id uuid, agent_id uuid, title text, status text, updated_at timestamptz, explicitly_created_at timestamptz) ON COMMIT DROP`,
				`CREATE TEMP TABLE member (workspace_id uuid, user_id uuid, role text) ON COMMIT DROP`,
				`CREATE TEMP TABLE agent (id uuid, owner_id uuid, permission_mode text) ON COMMIT DROP`,
				`CREATE TEMP TABLE agent_invocation_target (agent_id uuid, target_type text, target_id uuid) ON COMMIT DROP`,
				`CREATE TEMP TABLE agent_task_queue (id uuid, chat_session_id uuid, status text, regenerate_quick_actions_for uuid, priority int, created_at timestamptz) ON COMMIT DROP`,
				`CREATE TEMP TABLE chat_message (id uuid, chat_session_id uuid, task_id uuid, role text, message_kind text, created_at timestamptz, content text COLLATE ` + collation + `) ON COMMIT DROP`,
			}
			for _, stmt := range ddl {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					t.Skipf("collation %s unavailable: %v", collation, err)
				}
			}
			const (
				workspace = "00000000-0000-0000-0000-0000000000a1"
				user      = "00000000-0000-0000-0000-0000000000a2"
				agent     = "00000000-0000-0000-0000-0000000000a3"
				session   = "00000000-0000-0000-0000-0000000000a4"
				message   = "00000000-0000-0000-0000-0000000000a5"
			)
			seed := []string{
				`INSERT INTO member VALUES ('` + workspace + `', '` + user + `', 'member')`,
				`INSERT INTO agent VALUES ('` + agent + `', '` + user + `', 'private')`,
				`INSERT INTO chat_session VALUES ('` + session + `', '` + workspace + `', '` + user + `', '` + agent + `', 'unrelated title', 'active', now(), now())`,
				`INSERT INTO chat_message VALUES ('` + message + `', '` + session + `', NULL, 'user', 'text', now(), repeat(chr(304), 41) || ' needle at target')`,
			}
			for _, stmt := range seed {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					t.Fatalf("seed %q: %v", stmt, err)
				}
			}

			var snippet string
			err = tx.QueryRow(ctx, `/* `+collation+` keeps the prepared plan per subtest */ SELECT s.snippet FROM (`+chatSearchSQL+`) AS s(session_id, title, agent_id, status, updated_at, message_id, snippet)`,
				workspace, user, "needle", "%needle%", 10, 0).Scan(&snippet)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if !strings.Contains(snippet, "needle") {
				t.Fatalf("snippet lost the match: %q", snippet)
			}
		})
	}
}
