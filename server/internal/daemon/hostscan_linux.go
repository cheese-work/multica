//go:build linux

package daemon

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

var taskIDEnvPrefix = []byte("MULTICA_TASK_ID=")

// scanHostTaskIDs lists the MULTICA_TASK_ID of every live process on this host
// (CHE-5745). ok is false when the scan could not read a process it must.
func scanHostTaskIDs() ([]string, bool) { return scanProcTaskIDs("/proc") }

// scanProcTaskIDs reads <procRoot>/<pid>/environ of every same-uid process
// (a runner is always the daemon's own uid). A process that exits mid-scan is
// skipped; any other read error makes the scan not ok, because a task we could
// not look at must not be reported absent. ponytail: full /proc walk per
// heartbeat, cache the result if hosts with thousands of processes matter.
func scanProcTaskIDs(procRoot string) ([]string, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, false
	}
	uid := uint32(os.Getuid())
	ids := []string{}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		path := filepath.Join(procRoot, e.Name(), "environ")
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != uid {
			continue
		}
		raw, err := os.ReadFile(path)
		// Gone mid-scan, or a same-uid process we may not ptrace-read (it holds
		// capabilities we lack, e.g. `systemd --user`): neither is a runner, which
		// is our own descendant and so never has more privilege than this daemon.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) || errors.Is(err, fs.ErrPermission) {
			continue
		}
		if err != nil {
			return nil, false
		}
		for _, kv := range bytes.Split(raw, []byte{0}) {
			if id, ok := bytes.CutPrefix(kv, taskIDEnvPrefix); ok {
				ids = append(ids, string(id))
			}
		}
	}
	return ids, true
}
