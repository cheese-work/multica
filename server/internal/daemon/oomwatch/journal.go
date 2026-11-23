package oomwatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runner runs a program to completion and returns its stdout. The follower
// helpers take one so tests never start a real journalctl.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the Runner that starts the program.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

var errNoEntry = errors.New("journalctl returned no entry")

// lastEntry runs a read-only one-entry query and parses its first JSON line.
func lastEntry(ctx context.Context, run Runner, journalctl string, args ...string) (Entry, error) {
	out, err := run(ctx, journalctl, append([]string{"--no-pager", "--output=json", "-n", "1"}, args...)...)
	if err != nil {
		return Entry{}, err
	}
	line, _, _ := bytes.Cut(out, []byte("\n"))
	if len(bytes.TrimSpace(line)) == 0 {
		return Entry{}, errNoEntry
	}
	return ParseEntry(line)
}

// TailCursor returns the cursor of the newest journal entry of the boot: the
// baseline a tail start follows from.
func TailCursor(ctx context.Context, run Runner, journalctl, boot string) (string, error) {
	if !bootHex.MatchString(boot) {
		return "", fmt.Errorf("boot %q is not 32 lowercase hex", boot)
	}
	e, err := lastEntry(ctx, run, journalctl, "_BOOT_ID="+boot)
	if err != nil {
		return "", err
	}
	if e.BootID != boot {
		return "", fmt.Errorf("tail entry is from boot %q, want %q", e.BootID, boot)
	}
	if _, err := ParseCursor(e.Cursor); err != nil {
		return "", err
	}
	return e.Cursor, nil
}

// CursorValid reports whether the cursor still names an entry of the journal.
// `journalctl --cursor` seeks to the nearest entry when the cursor is gone, so
// the cursor is valid only when the entry it returns carries that very cursor.
// An error means the question could not be answered; callers treat it as not
// valid.
func CursorValid(ctx context.Context, run Runner, journalctl, boot, cursor string) (bool, error) {
	if !bootHex.MatchString(boot) {
		return false, fmt.Errorf("boot %q is not 32 lowercase hex", boot)
	}
	if _, err := ParseCursor(cursor); err != nil {
		return false, err
	}
	e, err := lastEntry(ctx, run, journalctl, "--cursor="+cursor, "_BOOT_ID="+boot)
	if errors.Is(err, errNoEntry) {
		return false, nil // a cursor past the end of the journal
	}
	if err != nil {
		return false, err
	}
	return e.Cursor == cursor, nil
}

// ErrFollowerExited is returned by Follow when journalctl ends on its own.
var ErrFollowerExited = errors.New("journalctl follower exited")

// Follow starts the production follower and feeds its output to the reader
// until the context ends or journalctl does. When journalctl ends on its own it
// returns ErrFollowerExited: the reader is blind from then on, so the caller
// must hold.
func Follow(parent context.Context, journalctl string, args []string, r *Reader, now func() time.Time) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	cmd := exec.CommandContext(ctx, journalctl, args...)
	cmd.WaitDelay = time.Second // do not wait on a grandchild that kept stderr open
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Closing the pipe unblocks the read even when a grandchild of journalctl
	// still holds the write end after the kill.
	stop := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	defer stop()
	runErr := r.Run(stdout, now)
	cancel() // a read failure must not leave the child running
	_ = cmd.Wait()
	switch {
	case parent.Err() != nil:
		return parent.Err()
	case runErr != nil:
		return runErr
	}
	return fmt.Errorf("%w: %s", ErrFollowerExited, strings.TrimSpace(stderr.String()))
}

// limitedWriter keeps the first n bytes.
type limitedWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		l.w.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
