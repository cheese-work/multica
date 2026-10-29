package handler

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Machine pause (CHE-2430). A daemon_pause row stops new task claims for every
// runtime on that daemon; running tasks finish.

func daemonPauseKey(workspaceID pgtype.UUID, daemonID string) string {
	return uuidToString(workspaceID) + "|" + daemonID
}

// pausedDaemonSet returns the keys of the paused daemons among the runtimes'
// (workspace, daemon) pairs. Runtimes with no daemon_id (cloud) are never paused.
func (h *Handler) pausedDaemonSet(ctx context.Context, runtimes []db.AgentRuntime) (map[string]struct{}, error) {
	var keys []string
	for _, rt := range runtimes {
		if rt.DaemonID.Valid {
			keys = append(keys, daemonPauseKey(rt.WorkspaceID, rt.DaemonID.String))
		}
	}
	paused := map[string]struct{}{}
	if len(keys) == 0 {
		return paused, nil
	}
	rows, err := h.Queries.ListPausedDaemons(ctx, keys)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		paused[daemonPauseKey(row.WorkspaceID, row.DaemonID)] = struct{}{}
	}
	return paused, nil
}

func runtimeOnPausedDaemon(paused map[string]struct{}, rt db.AgentRuntime) bool {
	if !rt.DaemonID.Valid {
		return false
	}
	_, ok := paused[daemonPauseKey(rt.WorkspaceID, rt.DaemonID.String)]
	return ok
}
