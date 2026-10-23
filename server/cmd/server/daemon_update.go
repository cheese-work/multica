package main

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"

	"github.com/multica-ai/multica/server/internal/handler"
)

// defaultDaemonBinariesDir is where the Dockerfile puts the daemon builds.
const defaultDaemonBinariesDir = "/app/daemon-binaries"

// loadDaemonUpdateSource returns the daemon builds this server serves to
// daemons, or nil when none match the server's own build commit (every
// update route then answers 404 and daemons keep the binary they run).
func loadDaemonUpdateSource() *handler.DaemonUpdateSource {
	dir := os.Getenv("MULTICA_DAEMON_BINARIES_DIR")
	if dir == "" {
		dir = defaultDaemonBinariesDir
	}
	src, err := handler.LoadDaemonUpdateSource(dir, commit)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			slog.Info("daemon update: no daemon binaries on this server", "dir", dir)
		} else {
			slog.Warn("daemon update: serving no daemon binaries", "dir", dir, "error", err)
		}
		return nil
	}
	return src
}
