//go:build linux

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// scope is a child cgroup created under the daemon's own cgroup. The command
// is placed in it at fork time (CLONE_INTO_CGROUP), so no child ever runs
// outside it.
type scope struct {
	path string
	dir  *os.File
}

// ownCgroup is the daemon's cgroup directory, where the daemon holds write
// authority (a delegated user@.service subtree).
func ownCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", rel), nil
		}
	}
	return "", errors.New("no cgroup v2 entry in /proc/self/cgroup")
}

func newScope(root string) (*scope, error) {
	if root == "" {
		var err error
		if root, err = ownCgroup(); err != nil {
			return nil, err
		}
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	s := &scope{path: filepath.Join(root, "multica-run-"+hex.EncodeToString(id))}
	if err := os.Mkdir(s.path, 0o755); err != nil {
		return nil, fmt.Errorf("create run cgroup: %w", err)
	}
	dir, err := os.Open(s.path)
	if err != nil {
		_ = os.Remove(s.path)
		return nil, err
	}
	s.dir = dir
	return s, nil
}

// attach makes cmd start inside the scope, in its own process group.
func (s *scope) attach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, UseCgroupFD: true, CgroupFD: int(s.dir.Fd())}
}

// releaseFD closes the directory handle once the child exists.
func (s *scope) releaseFD() {
	if s.dir != nil {
		_ = s.dir.Close()
		s.dir = nil
	}
}

// kill SIGKILLs every process in the scope, including re-parented ones.
func (s *scope) kill() error {
	return os.WriteFile(filepath.Join(s.path, "cgroup.kill"), []byte("1"), 0o644)
}

// close kills leftovers and removes the cgroup. Call it after Cmd.Wait: an
// unreaped zombie still counts as a member and keeps the cgroup alive.
func (s *scope) close() error {
	s.releaseFD()
	_ = s.kill()
	var err error
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if err = os.Remove(s.path); err == nil || os.IsNotExist(err) {
			return nil
		}
	}
	return fmt.Errorf("remove run cgroup: %w", err)
}
