//go:build linux

package daemon

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
// skipped only when the process is outside the daemon's service subtree (sshd,
// ssh-agent, systemd --user: never our runners); inside it, it may be a
// runner hiding its task id, so the scan is not ok. The subtree, not the exact
// cgroup: contained runs live in sibling leaves (CHE-7234). Any other read error is
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
	inService := serviceSubtree(self)
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
			if cerr != nil || inService(cg) {
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

// serviceSubtree returns the test "is this /proc/<pid>/cgroup content inside the
// daemon's service". The service root is the daemon's own cgroup, or its parent
// when the daemon sits in the delegated "daemon" subgroup (DelegateSubgroup=daemon,
// plan r13 3.1), where the run leaves are siblings under <unit>/runs. When a
// cgroup v2 path cannot be read from either file it falls back to the exact
// content match.
func serviceSubtree(self []byte) func([]byte) bool {
	exact := func(cg []byte) bool { return bytes.Equal(cg, self) }
	own, ok := unifiedCgroupPath(self)
	if !ok {
		return exact
	}
	root := own
	if i := strings.LastIndexByte(own, '/'); i > 0 && own[i+1:] == "daemon" {
		root = own[:i]
	}
	return func(cg []byte) bool {
		p, ok := unifiedCgroupPath(cg)
		if !ok {
			return exact(cg)
		}
		return p == root || strings.HasPrefix(p, root+"/")
	}
}

// unifiedCgroupPath reads the cgroup v2 ("0::<path>") line of a /proc/<pid>/cgroup.
func unifiedCgroupPath(b []byte) (string, bool) {
	for _, line := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(p), true
		}
	}
	return "", false
}
