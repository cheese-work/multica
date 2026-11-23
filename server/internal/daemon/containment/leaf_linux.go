//go:build linux

package containment

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const cgroupFS = "/sys/fs/cgroup"

// Options is the input to Create.
type Options struct {
	// Root is the directory that holds the attempt leaves (the runs/ cgroup).
	Root string
	// StateDir holds the attempt records and event logs.
	StateDir       string
	BootID         string // default: /proc/sys/kernel/random/boot_id
	DaemonInstance string
	TaskID         string
	// ClaimGeneration is the server's claim generation of the task (dispatched_at).
	ClaimGeneration string
	LaunchProfileID string
	// Charge is the memory charge, in bytes, held for the attempt.
	Charge int64
}

// Leaf is one attempt's cgroup leaf. It holds the directory fd that
// CLONE_INTO_CGROUP needs until End.
type Leaf struct {
	mu     sync.Mutex
	rec    Record
	dir    *os.File
	state  string
	events string
}

// ReadBootID returns this boot's ID.
func ReadBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b)), err
}

// DefaultRoot is the runs cgroup next to the daemon's own: with
// DelegateSubgroup=daemon the daemon sits in <unit>/daemon and the leaves go in
// <unit>/runs; without it they go in <own cgroup>/runs.
func DefaultRoot() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			own := filepath.Join(cgroupFS, rel)
			if filepath.Base(own) == "daemon" {
				return filepath.Join(filepath.Dir(own), "runs"), nil
			}
			return filepath.Join(own, "runs"), nil
		}
	}
	return "", errors.New("containment: no cgroup v2 entry in /proc/self/cgroup")
}

// Create makes the attempt's leaf and writes its fsynced record, in that order,
// before the caller can fork anything. On any failure nothing is left behind.
func Create(o Options) (*Leaf, error) {
	if !safeName.MatchString(o.TaskID) {
		return nil, fmt.Errorf("containment: unsafe task id %q", o.TaskID)
	}
	if o.Root == "" || o.StateDir == "" {
		return nil, errors.New("containment: root and state dir are required")
	}
	// The record is the authority for recovery: a missing identity or charge
	// fails closed before any leaf exists.
	switch {
	case o.ClaimGeneration == "":
		return nil, errors.New("containment: claim generation is required")
	case o.LaunchProfileID == "":
		return nil, errors.New("containment: launch profile id is required")
	case o.Charge <= 0:
		return nil, errors.New("containment: charge must be positive")
	}
	if o.BootID == "" {
		id, err := ReadBootID()
		if err != nil {
			return nil, fmt.Errorf("containment: boot id: %w", err)
		}
		o.BootID = id
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	attempt := o.TaskID + "-" + hex.EncodeToString(suffix)
	generation, err := nextGeneration(o.StateDir, o.TaskID)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(o.Root, 0o755); err != nil {
		return nil, fmt.Errorf("containment: runs cgroup: %w", err)
	}
	path := filepath.Join(o.Root, attempt)
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, fmt.Errorf("containment: create leaf: %w", err)
	}
	dir, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	fail := func(err error) (*Leaf, error) {
		dir.Close()
		_ = os.Remove(path)
		return nil, err
	}
	fi, err := dir.Stat()
	if err != nil {
		return fail(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return fail(errors.New("containment: cannot read the leaf's cgroup id"))
	}
	events := filepath.Join(o.StateDir, attempt+".events.jsonl")
	rec := Record{
		BootID: o.BootID, DaemonInstance: o.DaemonInstance, TaskID: o.TaskID,
		ClaimGeneration: o.ClaimGeneration, Attempt: attempt, Generation: generation,
		LaunchProfileID: o.LaunchProfileID, LeafPath: path, CgroupID: st.Ino, Charge: o.Charge,
		State: StateLeafCreated, EventsFile: events, CreatedAt: time.Now().UTC(),
	}
	if err := WriteRecord(o.StateDir, rec); err != nil {
		return fail(err)
	}
	return &Leaf{rec: rec, dir: dir, state: StateLeafCreated, events: events}, nil
}

// Record returns the attempt record as written at creation.
func (l *Leaf) Record() Record { return l.rec }

// EventsFile is the attempt's event log (blocked_launch events from the shims).
func (l *Leaf) EventsFile() string { return l.events }

// Attach makes cmd start inside the leaf (CLONE_INTO_CGROUP), so the child
// never runs outside it. It must be called before cmd.Start.
func (l *Leaf) Attach(cmd *exec.Cmd) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dir == nil {
		return errors.New("containment: leaf already ended")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(l.dir.Fd())
	return nil
}

// RecordClaim rewrites the durable record with a refreshed claim generation (a
// duplicate delivery during prep) before it returns, so the record holds the
// claim the next StartTask presents.
func (l *Leaf) RecordClaim(generation string) error {
	if generation == "" {
		return errors.New("containment: claim generation is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dir == nil {
		return errors.New("containment: leaf already ended")
	}
	if l.rec.ClaimGeneration == generation {
		return nil
	}
	next := l.rec
	next.ClaimGeneration = generation
	if err := WriteRecord(filepath.Dir(l.events), next); err != nil {
		return err
	}
	l.rec = next
	return nil
}

// Wrap is agent.LaunchHook: the agent CLI starts in the leaf.
func (l *Leaf) Wrap(cmd *exec.Cmd) error { return l.Attach(cmd) }

// End is the interim release: it confirms the leaf is still the one the record
// names (cgroup ID), kills what is left in it, removes it and marks the record
// ended. The identity-safe terminal sequence replaces it in step 11. Safe to
// call twice.
func (l *Leaf) End() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dir == nil {
		return nil
	}
	defer func() { l.dir.Close(); l.dir = nil }()
	fi, err := l.dir.Stat()
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Ino != l.rec.CgroupID {
		return errors.New("containment: leaf cgroup id no longer matches the record")
	}
	// cgroup.kill exists only in a real cgroup; never create it as a file.
	if k, err := os.OpenFile(filepath.Join(l.rec.LeafPath, "cgroup.kill"), os.O_WRONLY, 0); err == nil {
		_, _ = k.WriteString("1")
		k.Close()
	}
	var rmErr error
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if rmErr = os.Remove(l.rec.LeafPath); rmErr == nil || os.IsNotExist(rmErr) {
			rmErr = nil
			break
		}
	}
	if rmErr != nil {
		return fmt.Errorf("containment: remove leaf: %w", rmErr)
	}
	ended := l.rec
	ended.State = StateEnded
	return WriteRecord(filepath.Dir(l.events), ended)
}

// nextGeneration is one more than the attempt records already on disk for the
// task: the attempt's ordinal, so a re-delivered task never reuses one.
func nextGeneration(stateDir, taskID string) (int64, error) {
	matches, err := filepath.Glob(filepath.Join(stateDir, taskID+"-*.json"))
	if err != nil {
		return 0, err
	}
	n := int64(1)
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			return 0, fmt.Errorf("containment: read earlier record: %w", err)
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return 0, fmt.Errorf("containment: earlier record %s: %w", filepath.Base(m), err)
		}
		if r.TaskID == taskID {
			n++
		}
	}
	return n, nil
}
