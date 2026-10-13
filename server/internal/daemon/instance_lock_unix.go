//go:build !windows

package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockDaemonInstanceFile(lock *os.File) error {
	err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errDaemonInstanceLocked
	}
	return err
}
