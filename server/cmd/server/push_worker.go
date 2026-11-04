package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/pushnotify"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// startPushWorker runs the Web Push worker (CHE-1795) when the deployment has a
// VAPID identity. It returns nil when push is off, so no goroutine, no outbound
// client and no send path exists without keys.
func startPushWorker(ctx context.Context, pool *pgxpool.Pool, queries *db.Queries) *pushnotify.Worker {
	cfg := pushnotify.ConfigFromEnv()
	if !cfg.Enabled() {
		if cfg != (pushnotify.Config{}) {
			slog.Warn("web push disabled: MULTICA_VAPID_PUBLIC_KEY, MULTICA_VAPID_PRIVATE_KEY and MULTICA_VAPID_SUBJECT must all be set")
		}
		return nil
	}
	worker := pushnotify.NewWorker(queries, pool, pushnotify.NewWebPushSender(cfg, pushnotify.NewHTTPClient()))
	go worker.Run(ctx)
	return worker
}
