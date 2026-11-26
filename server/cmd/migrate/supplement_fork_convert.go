package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// forkDataCopyHooks maps an upstream-owned migration version to the hook that
// copies fork data into the table that migration builds on. The key is the
// target of the renumber-map entry for 662_chat_task_supplement_message: the
// hook runs after 99zzz01 created the table and before 99zzz02 builds the unique
// index on message_id. The registry scan reads no <digits><letters><digits>_
// key, so TestSupplementForkCopyHookIsRegisteredOnTheMapTarget guards the key.
var forkDataCopyHooks = map[string]preMigrationHook{
	"99zzz02_chat_task_supplement_message": runChatTaskSupplementForkCopyHook,
}

// chatTaskSupplementForkCounts partitions the fork rows: Copied (kept, whether
// inserted now or by an earlier run) + DroppedDuplicate + DiscardedNotDelivered
// equals the fork row count. Inserted is what this run wrote.
type chatTaskSupplementForkCounts struct {
	Copied                int
	DroppedDuplicate      int
	DiscardedNotDelivered int
	Inserted              int
}

func runChatTaskSupplementForkCopyHook(ctx context.Context, pool *pgxpool.Pool) error {
	counts, err := copyChatTaskSupplementFork(ctx, pool)
	if err != nil {
		return err
	}
	slog.Info("chat_task_supplement fork conversion",
		"copied", counts.Copied,
		"dropped_duplicate", counts.DroppedDuplicate,
		"discarded_not_delivered", counts.DiscardedNotDelivered,
		"inserted", counts.Inserted)
	return nil
}

// copyChatTaskSupplementFork copies delivered fork rows into upstream's table,
// one per message_id (earliest delivered_at, then lowest task_id). It skips a
// message that already has an upstream row, so a rerun inserts nothing. Without
// the renamed fork table it does nothing. The fork table is never written.
func copyChatTaskSupplementFork(ctx context.Context, pool *pgxpool.Pool) (chatTaskSupplementForkCounts, error) {
	var counts chatTaskSupplementForkCounts
	var forkExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('chat_task_supplement_fork_875') IS NOT NULL`).Scan(&forkExists); err != nil {
		return counts, fmt.Errorf("check chat_task_supplement fork table: %w", err)
	}
	if !forkExists {
		return counts, nil
	}
	err := pool.QueryRow(ctx, `
		WITH winners AS (
			SELECT DISTINCT ON (message_id)
				task_id, message_id, chat_session_id, workspace_id, author_id, created_at
			FROM chat_task_supplement_fork_875
			WHERE status = 'delivered'
			ORDER BY message_id, delivered_at ASC NULLS LAST, task_id ASC
		), inserted AS (
			INSERT INTO chat_task_supplement (task_id, message_id, chat_session_id, workspace_id, author_id, status, created_at)
			SELECT w.task_id, w.message_id, w.chat_session_id, w.workspace_id, w.author_id, 'delivered', w.created_at
			FROM winners w
			WHERE NOT EXISTS (SELECT 1 FROM chat_task_supplement c WHERE c.message_id = w.message_id)
			RETURNING 1
		), fork AS (
			SELECT count(*) FILTER (WHERE status = 'delivered') AS delivered, count(*) AS total
			FROM chat_task_supplement_fork_875
		)
		SELECT
			(SELECT count(*) FROM winners),
			(SELECT delivered FROM fork) - (SELECT count(*) FROM winners),
			(SELECT total - delivered FROM fork),
			(SELECT count(*) FROM inserted)`).
		Scan(&counts.Copied, &counts.DroppedDuplicate, &counts.DiscardedNotDelivered, &counts.Inserted)
	if err != nil {
		return counts, fmt.Errorf("copy delivered chat_task_supplement fork rows: %w", err)
	}
	return counts, nil
}
