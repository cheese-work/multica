package cardref

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// MinTokenLen is the shortest bearer token the pin channel accepts, as for the
// attestation channel (internal/doopread).
const MinTokenLen = 32

// maxBody caps a request body; the only body is {"reason": "..."}.
const maxBody = 4 << 10

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type statusJSON struct {
	Status Status `json:"status"`
}

type abortIfPendingJSON struct {
	Status     Status `json:"status"`
	NotAborted bool   `json:"notAborted"`
}

// NewHandler serves the three calls doop's `MulticaPort` makes, under
// /api/service/card-refs/{cardRequestId}/:
//
//	GET  status             -> {"status": "missing|pending|open|pick_pending|decided|void"}   (ref_status)
//	POST abort              {"reason": "..."} -> {"status": ...}                              (A1, force_release only)
//	POST abort-if-pending   -> {"status": ..., "notAborted": bool}                            (A2, reconcile only)
//
// Every call needs `Authorization: Bearer <token>`. A token shorter than
// MinTokenLen means the channel is not configured and every call is a 503.
// Nothing mounts this handler yet; the router wiring is its own change.
func NewHandler(db DB, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/service/card-refs/{id}/status", func(w http.ResponseWriter, r *http.Request) {
		id, ok := cardID(w, r)
		if !ok {
			return
		}
		s, err := RefStatus(r.Context(), db, id)
		reply(w, err, statusJSON{s})
	})
	mux.HandleFunc("POST /api/service/card-refs/{id}/abort", func(w http.ResponseWriter, r *http.Request) {
		id, ok := cardID(w, r)
		if !ok {
			return
		}
		var in struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil || strings.TrimSpace(in.Reason) == "" {
			http.Error(w, "reason is required", http.StatusBadRequest)
			return
		}
		s, err := AbortRef(r.Context(), db, id, in.Reason)
		reply(w, err, statusJSON{s})
	})
	mux.HandleFunc("POST /api/service/card-refs/{id}/abort-if-pending", func(w http.ResponseWriter, r *http.Request) {
		id, ok := cardID(w, r)
		if !ok {
			return
		}
		s, notAborted, err := AbortIfPending(r.Context(), db, id)
		reply(w, err, abortIfPendingJSON{s, notAborted})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(token) < MinTokenLen {
			http.Error(w, "card ref channel is not configured", http.StatusServiceUnavailable)
			return
		}
		want := "Bearer " + token
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func cardID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		http.Error(w, "invalid card request id", http.StatusBadRequest)
		return "", false
	}
	return id, true
}

func reply(w http.ResponseWriter, err error, body any) {
	if err != nil {
		http.Error(w, "card ref store unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
