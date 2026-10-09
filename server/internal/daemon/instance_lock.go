package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/cli"
)

var errDaemonInstanceLocked = errors.New("another daemon is already running (daemon instance lock is held)")

func acquireDaemonInstanceLock(profile string) (*os.File, error) {
	dir, err := cli.ProfileDir(profile)
	if err != nil {
		return nil, fmt.Errorf("resolve daemon instance lock directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create daemon instance lock directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon instance lock: %w", err)
	}
	if err := lockDaemonInstanceFile(lock); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("acquire daemon instance lock: %w", err)
	}
	return lock, nil
}
