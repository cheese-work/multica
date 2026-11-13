package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-4145 loader: chain identity, typed rows, marks, archived, and the C2 latest turn, against Postgres.
func TestLoadChatAttention(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	now := time.Now()

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	session := func(continuedFrom *string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO chat_session (workspace_id, agent_id, creator_id, continued_from_session_id, chain_root_session_id)
			VALUES ($1, $2, $3, $4::uuid, (SELECT chain_root_session_id FROM chat_session WHERE id = $4::uuid)) RETURNING id`,
			workspaceID, agentID, userID, continuedFrom).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Chain X: x1 continued into x2 (canonical). Chain Y: archived. Chain Z: two finished turns.
	x1 := session(nil)
	x2 := session(&x1)
	mustExec(`UPDATE chat_session SET continued_into_session_id = $2 WHERE id = $1`, x1, x2)
	y := session(nil)
	mustExec(`UPDATE chat_session SET status = 'archived' WHERE id = $1`, y)
	z := session(nil)
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM chat_attention_mark WHERE user_id = $1`, userID)
		pool.Exec(bg, `DELETE FROM chat_attention_item WHERE user_id = $1`, userID)
		pool.Exec(bg, `DELETE FROM decision_request WHERE human_user_id = $1`, userID)
		pool.Exec(bg, `DELETE FROM chat_turn_outcome WHERE user_id = $1`, userID)
		pool.Exec(bg, `DELETE FROM agent_task_queue WHERE chat_session_id = ANY($1)`, []string{x1, x2, y, z})
		pool.Exec(bg, `DELETE FROM chat_session WHERE id = ANY($1)`, []string{x1, x2, y, z})
	})

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	task := func(session, status string, age time.Duration) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, chat_session_id, created_at)
			VALUES ($1, $2, $3, 2, $4, $5) RETURNING id`, agentID, runtimeID, status, session, now.Add(-age)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	item := func(session, kind string, openedAgo time.Duration, dueIn *time.Duration, waitingTask *string) string {
		t.Helper()
		var id string
		var due *time.Time
		if dueIn != nil {
			d := now.Add(*dueIn)
			due = &d
		}
		var agent *string
		if kind == "ask" {
			agent = &agentID
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO chat_attention_item (workspace_id, chain_root_session_id, chat_session_id, user_id, agent_id, kind, opened_at, due_at,
				waiting_message_id, waiting_task_id)
			VALUES ($1, (SELECT chain_root_session_id FROM chat_session WHERE id = $2), $2, $3, $4, $5, $6, $7,
				CASE WHEN $8::uuid IS NULL THEN NULL ELSE gen_random_uuid() END, $8::uuid) RETURNING id`,
			workspaceID, session, userID, agent, kind, now.Add(-openedAgo), due, waitingTask).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	askID := item(x1, "ask", 5*time.Hour, nil, nil) // on the old DM of chain X: shows from x2
	laterDue := 2 * time.Hour
	laterID := item(x2, "later", 0, &laterDue, nil) // before due: hidden, drives next_change_at
	item(y, "ask", 30*time.Hour, nil, nil)          // archived chain
	var decisionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, escalation_class, origin, created_at)
		VALUES ($1, $2, $3, $4, $5, 'reversible', 'asked', $6) RETURNING id`,
		workspaceID, issueID, agentID, userID, x1, now.Add(-time.Hour)).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	// Finished turns are recorded when they end (CHE-4149); only the latest recorded turn can be unclassified.
	for _, id := range []string{task(z, "completed", 3*time.Hour), task(z, "completed", time.Hour)} {
		mustExec(`INSERT INTO chat_turn_outcome (task_id, workspace_id, chat_session_id, chain_root_session_id, user_id, kind) VALUES ($1, $4, $2, $2, $3, 'unclassified')`, id, z, userID, workspaceID)
	}
	latestZ := task(z, "completed", time.Hour/2)
	mustExec(`INSERT INTO chat_turn_outcome (task_id, workspace_id, chat_session_id, chain_root_session_id, user_id, kind) VALUES ($1, $4, $2, $2, $3, 'unclassified')`, latestZ, z, userID, workspaceID)
	task(z, "running", 0) // not finished: not a turn

	got, err := LoadChatAttention(ctx, q, util.MustParseUUID(workspaceID), util.MustParseUUID(userID), now)
	if err != nil {
		t.Fatal(err)
	}
	byCanon := map[string]ChatAttention{}
	for _, c := range got {
		byCanon[c.CanonicalID] = c
	}

	// Chain X reads from its canonical DM x2: ask (5h, yellow) and decision both show; later is hidden.
	cx, ok := byCanon[x2]
	if !ok {
		t.Fatalf("chain X missing: %+v", got)
	}
	wantKeys := []string{DecisionKey(decisionID, 1), ItemKey(askID)}
	slices.Sort(wantKeys)
	if !slices.Equal(cx.Keys, wantKeys) {
		t.Fatalf("X keys = %v, want %v", cx.Keys, wantKeys)
	}
	if cx.StaleLevel != AttentionYellow || cx.Counts.Ask != 1 || cx.Counts.Decision != 1 || cx.Counts.Later != 0 {
		t.Fatalf("X = %+v", cx)
	}
	if cx.NextChangeAt == nil || cx.NextChangeAt.Sub(now.Add(laterDue)).Abs() > time.Second {
		t.Fatalf("X next_change_at = %v, want ~due of %s", cx.NextChangeAt, laterID)
	}
	if _, ok := byCanon[x1]; ok {
		t.Fatalf("old DM x1 listed separately")
	}
	if got, ok := byCanon[y]; ok { // A14: absent from the list, not merely empty
		t.Fatalf("archived chain listed: %+v", got)
	}

	// C2: only the latest finished turn of Z, uncoloured.
	cz := byCanon[z]
	if !slices.Equal(cz.Unclassified, []string{TurnKey(latestZ)}) || cz.StaleLevel != AttentionNone {
		t.Fatalf("Z = %+v, want only latest turn %s", cz, latestZ)
	}

	// Marks apply by key from any DM: snooze the ask, ack the turn.
	mustExec(`INSERT INTO chat_attention_mark (user_id, subject_key, chain_root_session_id, snoozed_until, acked_at) VALUES
		($1, $2, $4, $5, NULL), ($1, $3, $6, NULL, now())`,
		userID, ItemKey(askID), TurnKey(latestZ), x1, now.Add(time.Hour), z)
	got, err = LoadChatAttention(ctx, q, util.MustParseUUID(workspaceID), util.MustParseUUID(userID), now)
	if err != nil {
		t.Fatal(err)
	}
	byCanon = map[string]ChatAttention{}
	for _, c := range got {
		byCanon[c.CanonicalID] = c
	}
	if !slices.Equal(byCanon[x2].Keys, []string{DecisionKey(decisionID, 1)}) {
		t.Fatalf("X after snooze = %v", byCanon[x2].Keys)
	}
	if len(byCanon[z].Unclassified) != 0 {
		t.Fatalf("Z after ack = %+v", byCanon[z])
	}
}
