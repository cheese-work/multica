package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Owner maintenance window.
//
// An operator who must replace the daemon binary or its environment needs the
// daemon to claim nothing while they do it. The primitives already exist —
// `updating` is the update-ownership flag, pauseClaims/claimsInFlight/
// activeTasks the claim barrier, and tryBeginServerUpdate acquires both
// atomically, waiting out claims already in flight. The window is that same
// acquisition held by an external owner instead of by handleUpdate, which is
// why auto-update, auto-reload, server-triggered updates and runtime demotion
// all refuse to run while it is held without any change to them.
//
// Fail-closed rules:
//   - Held only in memory and bound to this process. If the daemon dies the
//     window dies with it and the successor claims normally; the owner learns
//     that from /health (maintenance_held=false) or a refused call, never from
//     silence.
//   - A lease TTL bounds a window whose owner vanished, so a crashed script
//     cannot leave a live daemon claiming nothing forever.
//   - While the window is held, /shutdown (explicit stop/restart) is refused
//     unless it carries the window's token. A caller that presents a token for
//     a window that is no longer held is also refused: its exclusivity
//     assumption is false and it must reconcile before acting.
//   - Once an authorized shutdown is accepted the lease stops expiring and
//     release is refused, so claims stay paused until the process exits.
//
// Limits: the listener is loopback-only and unauthenticated by design (same
// trust boundary as /shutdown); the token proves ownership of the window, not
// the caller's identity. A SIGINT/SIGTERM sent straight to the process is not
// an explicit restart and is not gated.
const (
	maintenanceDefaultTTL     = 15 * time.Minute
	maintenanceMaxTTL         = time.Hour
	maintenanceAcquireTimeout = 30 * time.Second
	maintenanceTokenHeader    = "X-Maintenance-Token"
)

type maintenanceLease struct {
	token   string
	expires time.Time
	timer   *time.Timer
	closing bool // an authorized shutdown was accepted; lease is final
}

type maintenanceError string

func (e maintenanceError) Error() string { return string(e) }

const (
	errMaintenanceBusy          maintenanceError = "busy"            // work or a claim is in flight
	errMaintenanceUpdateRunning maintenanceError = "update_running"  // another update or window owns update ownership
	errMaintenanceRestarting    maintenanceError = "restart_pending" // a restart is already scheduled
	errMaintenanceNotHeld       maintenanceError = "not_held"        // no window (never held, released or expired)
	errMaintenanceTokenMismatch maintenanceError = "token_mismatch"  // a window is held, by someone else
	errMaintenanceClosing       maintenanceError = "shutting_down"   // window is final: shutdown accepted
)

// acquireMaintenance takes the window, returning its secret token and expiry.
// ttl is clamped to (0, maintenanceMaxTTL]; zero selects the default.
func (d *Daemon) acquireMaintenance(ctx context.Context, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = maintenanceDefaultTTL
	}
	if ttl > maintenanceMaxTTL {
		ttl = maintenanceMaxTTL
	}
	if d.RestartBinary() != "" {
		return "", time.Time{}, errMaintenanceRestarting
	}
	switch d.tryBeginServerUpdate(ctx) {
	case serverUpdateAlreadyRunning:
		return "", time.Time{}, errMaintenanceUpdateRunning
	case serverUpdateRuntimeBusy:
		return "", time.Time{}, errMaintenanceBusy
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		d.releaseClaimBarrier()
		d.updating.Store(false)
		return "", time.Time{}, err
	}
	lease := &maintenanceLease{token: hex.EncodeToString(raw), expires: time.Now().Add(ttl)}
	d.maintMu.Lock()
	d.maint = lease
	lease.timer = time.AfterFunc(ttl, func() { _ = d.releaseMaintenance(lease.token) })
	d.maintMu.Unlock()
	d.logger.Info("maintenance window acquired", "expires_at", lease.expires.UTC().Format(time.RFC3339))
	return lease.token, lease.expires, nil
}

// releaseMaintenance ends the window and resumes claiming.
func (d *Daemon) releaseMaintenance(token string) error {
	d.maintMu.Lock()
	defer d.maintMu.Unlock()
	lease := d.maint
	switch {
	case lease == nil:
		return errMaintenanceNotHeld
	case !tokenEqual(lease.token, token):
		return errMaintenanceTokenMismatch
	case lease.closing:
		return errMaintenanceClosing
	}
	lease.timer.Stop()
	d.maint = nil
	d.releaseClaimBarrier()
	d.updating.Store(false)
	d.logger.Info("maintenance window released")
	return nil
}

// authorizeShutdown decides whether /shutdown may proceed. With no window and
// no token it is the historical unconditional shutdown. On success while a
// window is held the lease is frozen (no expiry, no release) so claims stay
// paused until exit.
func (d *Daemon) authorizeShutdown(token string) error {
	d.maintMu.Lock()
	defer d.maintMu.Unlock()
	lease := d.maint
	switch {
	case lease == nil && token == "":
		return nil
	case lease == nil:
		return errMaintenanceNotHeld
	case !tokenEqual(lease.token, token):
		return errMaintenanceTokenMismatch
	}
	lease.closing = true
	lease.timer.Stop()
	return nil
}

// maintenanceStatus reports window state for /health. The token is never
// included.
func (d *Daemon) maintenanceStatus() (held bool, expires time.Time) {
	d.maintMu.Lock()
	defer d.maintMu.Unlock()
	if d.maint == nil {
		return false, time.Time{}
	}
	return true, d.maint.expires
}

func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func maintenanceHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errMaintenanceTokenMismatch):
		return http.StatusForbidden
	case errors.As(err, new(maintenanceError)):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeMaintenanceError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(maintenanceHTTPStatus(err))
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// maintenanceAcquireHandler serves POST /maintenance/acquire.
// Body (optional): {"ttl_seconds": N}.
func (d *Daemon) maintenanceAcquireHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			TTLSeconds int `json:"ttl_seconds"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TTLSeconds < 0 {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), maintenanceAcquireTimeout)
		defer cancel()
		token, expires, err := d.acquireMaintenance(ctx, time.Duration(req.TTLSeconds)*time.Second)
		if err != nil {
			writeMaintenanceError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      token,
			"expires_at": expires.UTC().Format(time.RFC3339),
		})
	}
}

// maintenanceReleaseHandler serves POST /maintenance/release. Body: {"token": "..."}.
func (d *Daemon) maintenanceReleaseHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
			http.Error(w, "token is required", http.StatusBadRequest)
			return
		}
		if err := d.releaseMaintenance(req.Token); err != nil {
			writeMaintenanceError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "released"})
	}
}
