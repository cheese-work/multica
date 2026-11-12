package cibroker

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

const maxBody = 4 << 10 // rule 2: a larger request body is a 413

// Handler serves POST /api/ci/github-token behind the ci_token_broker flag.
// S1 authorizes only; it answers 501 where S2 will mint a token. It never logs
// or echoes the JWT.
type Handler struct {
	Flags  *featureflag.Service
	Broker *Broker
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !featureflags.CITokenBrokerEnabled(r.Context(), h.Flags) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		reply(w, 405, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if r.ContentLength > maxBody {
		reply(w, 413, "request body too large")
		return
	}
	if _, err := io.ReadAll(r.Body); err != nil {
		reply(w, 413, "request body too large")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		reply(w, 401, "missing bearer token")
		return
	}
	if _, _, err := h.Broker.Authorize(r.Context(), token); err != nil {
		var ref *Refusal
		if errors.As(err, &ref) {
			reply(w, ref.Status, ref.Reason)
			return
		}
		reply(w, 500, "internal error")
		return
	}
	reply(w, 501, "token issuance not implemented") // S2
}

func reply(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
