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

// scanProcTaskIDs reads <procRoot>/<pid>/environ of every process of this uid
// (a runner is always the daemon's own uid). The uid comes from status, not
// from environ's owner: a nondumpable process has a root-owned environ.
// A process that exits mid-scan is skipped. An environ we may not read is
// skipped only when the process is outside the daemon's cgroup (sshd,
// ssh-agent, systemd --user: never our runners); inside it, it may be a
// runner hiding its task id, so the scan is not ok. Any other read error is
// also not ok: a task we could not look at must not be reported absent.
// ponytail: full /proc walk per heartbeat, cache if hosts with thousands of
// processes matter.
func scanProcTaskIDs(procRoot string) ([]string, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, false
	}
	self, err := os.ReadFile(filepath.Join(procRoot, "self", "cgroup"))
	if err != nil {
		return nil, false
	}
	uid := os.Getuid()
	ids := []string{}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		owner, err := procUID(dir)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, false
		}
		if owner != uid {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "environ"))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if errors.Is(err, fs.ErrPermission) {
			cg, cerr := os.ReadFile(filepath.Join(dir, "cgroup"))
			if errors.Is(cerr, fs.ErrNotExist) || errors.Is(cerr, syscall.ESRCH) {
				continue
			}
			if cerr != nil || bytes.Equal(cg, self) {
				return nil, false
			}
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

// procUID reads the real uid from <dir>/status (world-readable).
func procUID(dir string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return 0, err
	}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if rest, ok := bytes.CutPrefix(line, []byte("Uid:")); ok {
			f := bytes.Fields(rest)
			if len(f) == 0 {
				break
			}
			return strconv.Atoi(string(f[0]))
		}
	}
	return 0, errors.New("no Uid line in " + dir)
}
