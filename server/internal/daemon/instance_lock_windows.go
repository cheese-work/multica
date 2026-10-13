//go:build windows

package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lockDaemonInstanceFile(lock *os.File) error {
	err := windows.LockFileEx(windows.Handle(lock.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errDaemonInstanceLocked
	}
	return err
}
