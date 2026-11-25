// Package containment is the per-attempt cgroup leaf behind the containment
// flag (CHE-7168 plan r13 3.2, step 5 / S1a-1). A contained attempt gets one
// leaf cgroup, created before any process of the attempt exists, with a
// durable record of the leaf's cgroup ID. Prep processes and the agent CLI are
// forked straight into the leaf with CLONE_INTO_CGROUP, so no process of the
// attempt ever runs outside it.
//
// Identity-safe terminal cleanup, the restart reconcile and admission are
// later steps (11, 12, 7 and 8); End here is the interim release.
package containment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Attempt record states. Later steps add the rest of the r3 list.
const (
	StateLeafCreated = "leaf_created"
	StateEnded       = "ended"
)

// Record is the durable attempt record (plan r3 3.2 item 1). Authority to clean
// is the record plus a matching CgroupID; a path, an environment tag or a PID
// is evidence for alerts only.
type Record struct {
	BootID          string    `json:"boot_id"`
	DaemonInstance  string    `json:"daemon_instance"`
	TaskID          string    `json:"task_id"`
	ClaimGeneration string    `json:"claim_generation"`
	Attempt         string    `json:"attempt"`
	Generation      int64     `json:"generation"`
	LaunchProfileID string    `json:"launch_profile_id"`
	LeafPath        string    `json:"leaf_path"`
	CgroupID        uint64    `json:"cgroup_id"`
	Charge          int64     `json:"charge"`
	State           string    `json:"state"`
	EventsFile      string    `json:"events_file,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func recordPath(dir, attempt string) (string, error) {
	if !safeName.MatchString(attempt) {
		return "", fmt.Errorf("containment: unsafe attempt name %q", attempt)
	}
	return filepath.Join(dir, attempt+".json"), nil
}

// WriteRecord replaces the attempt's record atomically: temporary file, fsync,
// rename, directory fsync. A reader sees the old record or the new one.
func WriteRecord(dir string, r Record) error {
	path, err := recordPath(dir, r.Attempt)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("containment: record dir: %w", err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".record-*.tmp")
	if err != nil {
		return fmt.Errorf("containment: record temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ReadRecord reads one attempt's record.
func ReadRecord(dir, attempt string) (Record, error) {
	path, err := recordPath(dir, attempt)
	if err != nil {
		return Record{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, fmt.Errorf("containment: record %s: %w", attempt, err)
	}
	return r, nil
}

// ErrUnsupported is returned where run cgroups do not exist (not Linux).
var ErrUnsupported = errors.New("containment: needs Linux cgroup v2")
