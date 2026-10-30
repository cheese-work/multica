//go:build linux

package sandbox

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// pin opens p as an O_PATH descriptor, which fixes the inode that bwrap will
// mount, and returns the real path that descriptor refers to for deny checks.
func pin(p string) (*os.File, string, error) {
	fd, err := unix.Open(p, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", &os.PathError{Op: "open", Path: p, Err: err}
	}
	real, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		unix.Close(fd)
		return nil, "", err
	}
	return os.NewFile(uintptr(fd), p), real, nil
}
