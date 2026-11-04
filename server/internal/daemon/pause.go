package daemon

import (
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Server-owned machine pause (CHE-2673 L2). The server already refuses to hand
// this machine a task while paused; the daemon mirrors the pause from the
// heartbeat ack so it also stops asking, and reports its in-flight count so
// the server can tell the owner the machine is safe to shut down.
//
// Unlike the owner maintenance window (maintenance.go) this is not a barrier:
// it takes no lease, blocks no update, and never stops the process. It is
// cleared by the first ack without a pause, and lapses on its own at
// expires_at so a daemon cut off from the server cannot stay paused forever.
type heldPause struct {
	reason    string
	expiresAt time.Time
}

// applyHeartbeatPause records the pause carried by an ack, or clears it when
// the ack carries none. An unparseable expiry is treated as no pause: failing
// open keeps a malformed ack from idling the machine.
func (d *Daemon) applyHeartbeatPause(p *protocol.DaemonHeartbeatPaused) {
	if p == nil {
		d.pause.Store(nil)
		return
	}
	expires, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil {
		d.pause.Store(nil)
		return
	}
	d.pause.Store(&heldPause{reason: p.Reason, expiresAt: expires})
}

// heldPauseNow returns the live pause, or nil when none is held or it expired.
func (d *Daemon) heldPauseNow() *heldPause {
	p := d.pause.Load()
	if p == nil || !time.Now().Before(p.expiresAt) {
		return nil
	}
	return p
}
