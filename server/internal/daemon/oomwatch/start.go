package oomwatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Checkpoint is the minimal persisted reader state of step 9a. Step 9c extends
// it (B, R, key set, deficit clock, boundary state).
type Checkpoint struct {
	BootID         string    `json:"boot_id"`
	Cursor         string    `json:"cursor"`
	HeartbeatNonce string    `json:"heartbeat_nonce"`
	HeartbeatAt    time.Time `json:"heartbeat_at"`
}

// StartMode says where the follower begins.
type StartMode string

const (
	StartTail   StartMode = "tail"   // baseline at the journal tail
	StartResume StartMode = "resume" // --after-cursor the checkpoint cursor
)

// StartInput is what the reader knows before it follows.
type StartInput struct {
	Boot           string
	Checkpoint     *Checkpoint // nil when no checkpoint file exists
	CheckpointTorn bool        // the file exists but cannot be read
	CursorValid    bool        // the checkpoint cursor is still in the journal
	Now            time.Time
}

// StartPlan is the decision at reader start.
type StartPlan struct {
	Mode            StartMode
	Cursor          string
	Gap             *GapEvent
	Hold            *HoldEvent
	RestoreCounters bool // only a same-boot resume with a valid cursor may restore counter state
}

// resumeGapAfter is two heartbeat periods: a restart that took longer leaves a
// telemetry gap even when the cursor still resumes.
const resumeGapAfter = 2 * HeartbeatPeriod

// PlanStart applies the cursor and boot-gap rules.
//
//   - No checkpoint: first start, follow from the tail.
//   - Torn checkpoint: tail, gap and hold.
//   - Another boot: tail of the new boot, gap, nothing restored.
//   - Same boot, cursor still in the journal: resume after it; a gap only when
//     the last heartbeat is more than 60 s old.
//   - Same boot, cursor gone: tail, gap and a live_gap hold, nothing restored.
func PlanStart(in StartInput) StartPlan {
	gap := func(reason GapReason, from time.Time) *GapEvent {
		return &GapEvent{Reason: reason, From: from, To: in.Now}
	}
	cp := in.Checkpoint
	switch {
	case in.CheckpointTorn:
		return StartPlan{
			Mode: StartTail,
			Gap:  gap(GapCheckpointTorn, time.Time{}),
			Hold: &HoldEvent{Cause: HoldCheckpointTorn, Detail: "checkpoint file cannot be read back whole", At: in.Now},
		}
	case cp == nil:
		return StartPlan{Mode: StartTail}
	case cp.BootID != in.Boot:
		return StartPlan{Mode: StartTail, Gap: gap(GapBootChange, cp.HeartbeatAt)}
	case !in.CursorValid:
		return StartPlan{
			Mode: StartTail,
			Gap:  gap(GapCursorInvalid, cp.HeartbeatAt),
			Hold: &HoldEvent{Cause: HoldLiveGap, Detail: "checkpoint cursor is no longer in the journal", At: in.Now},
		}
	}
	p := StartPlan{Mode: StartResume, Cursor: cp.Cursor, RestoreCounters: true}
	if in.Now.Sub(cp.HeartbeatAt) > resumeGapAfter {
		p.Gap = gap(GapResume, cp.HeartbeatAt)
	}
	return p
}

// WriteCheckpoint writes the checkpoint atomically: temporary file, fsync,
// rename, directory fsync. A failure leaves the previous file and no
// temporary file behind.
func WriteCheckpoint(path string, cp Checkpoint) (err error) {
	body, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp") // mode 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(append(body, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ReadCheckpoint reads the checkpoint. A missing file is (nil, false, nil). A
// file that cannot be read back whole is (nil, true, nil): torn. A read error
// that says nothing about the content (permissions, I/O) is returned as err.
func ReadCheckpoint(path string) (cp *Checkpoint, torn bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Checkpoint
	if err := dec.Decode(&c); err != nil {
		return nil, true, nil
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) { // trailing data
		return nil, true, nil
	}
	if !bootHex.MatchString(c.BootID) {
		return nil, true, nil
	}
	if _, err := ParseCursor(c.Cursor); err != nil {
		return nil, true, nil
	}
	return &c, false, nil
}
