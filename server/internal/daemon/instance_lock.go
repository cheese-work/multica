package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

var errDaemonInstanceLocked = errors.New("another daemon is already running (daemon instance lock is held)")

func WaitForInstanceRelease(ctx context.Context, profile string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lock, err := acquireDaemonInstanceLock(profile)
		if err == nil {
			return lock.Close()
		}
		if !errors.Is(err, errDaemonInstanceLocked) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

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
