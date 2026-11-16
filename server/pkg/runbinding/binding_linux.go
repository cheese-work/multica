package runbinding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Publisher belongs to one launch attempt. Retries must allocate a new one.
type Publisher struct {
	ctx       context.Context
	ID        string
	spec      Spec
	root      *os.Root
	mu        sync.Mutex
	closed    bool
	published bool
	stop      func() bool
}

func New(ctx context.Context, spec Spec) (*Publisher, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !filepath.IsAbs(spec.Dir) || spec.AgentID == "" || spec.TaskID == "" || spec.IssueID == "" {
		return nil, errors.New("managed binding requires an absolute registry and task, agent and issue identity")
	}
	info, err := os.Lstat(spec.Dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("managed binding registry must be a private 0700 directory")
	}
	root, err := os.OpenRoot(spec.Dir)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, errors.New("managed binding registry owner or identity mismatch")
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		_ = root.Close()
		return nil, errors.New("managed binding registry owner or identity mismatch")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = root.Close()
		return nil, err
	}
	p := &Publisher{ctx: ctx, ID: hex.EncodeToString(nonce[:]), spec: spec, root: root}
	// Assign under the same mutex used by Close: cancellation may already fire.
	p.mu.Lock()
	p.stop = context.AfterFunc(ctx, func() { p.Close() })
	p.mu.Unlock()
	return p, nil
}

// Publish observes only the actual launched process root. Exec wrappers retain
// this PID; waiting wrappers are holders only while alive. Detached children do
// not extend the binding. Never pass a daemon PID or a namespace PID hint.
func (p *Publisher) Publish(pid int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if p.closed || p.published {
		return errors.New("managed binding is closed or already published")
	}
	if pid <= 1 || pid == os.Getpid() {
		return errors.New("managed binding requires a launched child holder")
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return errors.New("invalid holder stat")
	}
	fields := strings.Fields(string(stat)[end+1:])
	if len(fields) <= 19 || fields[0] == "Z" || fields[0] == "X" {
		return errors.New("holder is dead or malformed")
	}
	// Only our direct child is eligible, including exec/wait wrappers.
	if fields[1] != strconv.Itoa(os.Getpid()) {
		return errors.New("holder is not a launcher child")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return err
	}
	record := map[string]any{"version": 1, "binding_id": p.ID, "agent_id": p.spec.AgentID, "principal": p.spec.AgentID, "task_id": p.spec.TaskID, "issue_id": p.spec.IssueID, "pid": pid, "process_start": fields[19], "boot_id": strings.TrimSpace(string(boot)), "pid_namespace": ns}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp := p.ID + ".tmp"
	f, err := p.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	// A temporary file is never a usable binding, including after a failed write.
	defer func() { _ = p.root.Remove(temp) }()
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := p.root.Rename(temp, p.ID+".json"); err != nil {
		return err
	}
	p.published = true
	return nil
}

// Close revokes before process cleanup so cancellation cannot preserve a lease.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.stop != nil {
		p.stop()
	}
	if err := p.root.Remove(p.ID + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Process cleanup must continue; broker liveness still fences a dead holder.
		slog.Error("managed binding revocation failed", "error", err)
	}
	_ = p.root.Close()
}
