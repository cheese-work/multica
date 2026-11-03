package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Authority is the server side of per-operation authority (CHE-2216 plan 3.8).
// The caller binds the task and epoch.
type Authority interface {
	// Authorize records the exact input about to run and returns the operation ID.
	Authorize(ctx context.Context, scope, inputHash string) (opID string, err error)
	// Complete reports how an authorized operation ended: completed, failed or aborted.
	Complete(ctx context.Context, opID, result string) error
}

// Elevation makes every push and PR operation of a run an authorized one: the
// broker hashes the full input, authorizes it, runs exactly that input and
// reports the result. Fence then stops new work and drains what is in flight.
type Elevation struct {
	Authority Authority
	Scope     string
	// ReportFor bounds the retries of one result report; zero means 30 seconds.
	// A report still unacknowledged then leaves Drain failing closed.
	ReportFor time.Duration
}

// ErrUnreported means an operation ended but the server never acknowledged its
// result, so the fence cannot complete (plan 3.8, fail closed).
var ErrUnreported = errors.New("broker: operation result not acknowledged")

// ErrOutcomeUnknown means a remote write started and ended in a way that does
// not show whether the remote applied it: a timeout, a lost reply, a server
// error. The broker reports nothing for it, so the server's lease lapses to
// unknown and the fence stays closed until the outcome is reconciled.
var ErrOutcomeUnknown = errors.New("broker: remote write outcome unknown")

// notApplied marks an error that proves the remote did not apply the write: a
// rejected ref, a 4xx answer. Any other error after the write started may have
// landed.
type notApplied struct{ error }

func (e notApplied) Unwrap() error { return e.error }

type elevOp struct {
	id      string
	started bool // the remote write began; the fence lets it finish
	ended   bool
	unknown bool // the write ended with no proof either way; nothing is reported
	acked   bool
	result  string
}

// inputHash binds an authorization to the exact input: length-prefixed parts,
// so no two inputs share a hash by moving bytes between fields.
func inputHash(kind string, parts ...string) string {
	h := sha256.New()
	for _, p := range append([]string{kind}, parts...) {
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Fence switches the run to restricted at once, with no server round trip: no
// new operation is authorized, and one that has not started its remote write
// aborts when it reaches it. Started writes finish. Safe to call repeatedly.
func (b *Broker) Fence() {
	b.fmu.Lock()
	b.fenced = true
	b.fmu.Unlock()
	b.notify()
}

// Drain returns once every operation has ended and the server has acknowledged
// its result. It retries unacknowledged reports; one that still fails returns
// ErrUnreported, an operation whose write may have landed returns
// ErrOutcomeUnknown, and ctx expiry returns ctx's error: the fence is not
// complete. Reconciling an unknown outcome is not the broker's job.
func (b *Broker) Drain(ctx context.Context) error {
	for {
		b.fmu.Lock()
		running, unknown, stuck, changed := 0, 0, []*elevOp(nil), b.changed
		for o := range b.open {
			switch {
			case !o.ended:
				running++
			case o.unknown:
				unknown++
			case !o.acked:
				stuck = append(stuck, o)
			}
		}
		b.fmu.Unlock()
		progress := false
		for _, o := range stuck {
			if b.deliver(ctx, o) == nil {
				progress = true
			}
		}
		switch {
		case progress:
		case running > 0:
			select {
			case <-changed:
			case <-ctx.Done():
				return fmt.Errorf("broker: fence not drained: %w", ctx.Err())
			}
		case len(stuck) > 0:
			return ErrUnreported
		case unknown > 0:
			return ErrOutcomeUnknown
		default:
			return nil
		}
	}
}

func (b *Broker) notify() {
	b.fmu.Lock()
	close(b.changed)
	b.changed = make(chan struct{})
	b.fmu.Unlock()
}

// authorize runs the authority check for input and registers the operation. A
// run with no Elevation gets a nil op, whose methods do nothing. It returns the
// HTTP status to answer with when the operation must not run. The operation is
// open from before the server call, so Drain waits for an authorization in
// flight.
func (b *Broker) authorize(ctx context.Context, input string) (*elevOp, int, error) {
	el := b.pin.Elevation
	if el == nil {
		return nil, 0, nil
	}
	errFenced := errors.New("run is fenced: elevated operations are refused")
	o := &elevOp{}
	b.fmu.Lock()
	if b.fenced {
		b.fmu.Unlock()
		return nil, http.StatusForbidden, errFenced
	}
	b.open[o] = struct{}{}
	b.fmu.Unlock()
	id, err := el.Authority.Authorize(ctx, el.Scope, input)
	if err != nil {
		b.log.Warn("broker authorize failed; refusing", "task", b.pin.Task, "error", err)
		b.fmu.Lock()
		delete(b.open, o)
		b.fmu.Unlock()
		b.notify()
		return nil, http.StatusServiceUnavailable, errors.New("no authority for this operation; refused")
	}
	b.fmu.Lock()
	o.id = id
	if b.fenced { // the fence began while the server was authorizing
		o.result, o.ended = "aborted", true
		b.fmu.Unlock()
		b.report(o)
		return nil, http.StatusForbidden, errFenced
	}
	b.fmu.Unlock()
	return o, 0, nil
}

// write is called right before the remote write. After the fence it refuses, so
// the operation aborts; otherwise the write counts as started.
func (b *Broker) write(o *elevOp) error {
	if o == nil {
		return nil
	}
	if b.beforeWrite != nil {
		b.beforeWrite()
	}
	b.fmu.Lock()
	defer b.fmu.Unlock()
	if b.fenced {
		return errors.New("run is fenced: operation aborted before its remote write")
	}
	o.started = true
	return nil
}

// finish reports how the operation ended: completed, aborted if the write never
// started, failed if the remote provably did not apply it. Any other error
// after the write started reports nothing, so the server's lease lapses to
// unknown and the fence stays closed.
func (b *Broker) finish(o *elevOp, err error) {
	if o == nil {
		return
	}
	if b.afterWrite != nil && err == nil {
		b.afterWrite()
	}
	b.fmu.Lock()
	o.ended = true
	switch {
	case err == nil:
		o.result = "completed"
	case !o.started:
		o.result = "aborted"
	case errors.As(err, new(notApplied)):
		o.result = "failed"
	default:
		o.unknown = true
	}
	unknown := o.unknown
	b.fmu.Unlock()
	if unknown {
		b.log.Error("broker remote write outcome unknown; reporting nothing", "task", b.pin.Task, "op", o.id, "error", err)
		b.notify()
		return
	}
	b.report(o)
}

// report retries the result report for ReportFor. On failure the operation
// stays open and unacknowledged, so Drain fails closed.
func (b *Broker) report(o *elevOp) {
	ctx, cancel := context.WithTimeout(context.Background(), or(b.pin.Elevation.ReportFor, 30*time.Second))
	defer cancel()
	for wait := 10 * time.Millisecond; ; wait = min(wait*2, 2*time.Second) {
		if b.deliver(ctx, o) == nil {
			break
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			b.log.Error("broker could not report an operation result", "task", b.pin.Task, "op", o.id, "result", o.result)
			b.notify()
			return
		}
	}
}

func (b *Broker) deliver(ctx context.Context, o *elevOp) error {
	if err := b.pin.Elevation.Authority.Complete(ctx, o.id, o.result); err != nil {
		return err
	}
	b.fmu.Lock()
	o.acked = true
	delete(b.open, o)
	b.fmu.Unlock()
	b.notify()
	return nil
}
