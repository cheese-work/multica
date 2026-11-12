package cibroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/cibroker/mint"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

const maxBody = 4 << 10 // rule 2: a larger request body is a 413

// TokenIssuer mints a token for a verified, policy-matched request (slice S2).
type TokenIssuer interface {
	Issue(ctx context.Context, req mint.Request) (mint.Token, error)
}

// Handler serves POST /api/ci/github-token behind the ci_token_broker flag.
// It authorizes the JWT (S1), then asks Mint for a token (S2). Without a Mint
// it answers 501. It never logs or echoes the JWT.
type Handler struct {
	Flags  *featureflag.Service
	Broker *Broker
	Mint   TokenIssuer
}

// tokenRequest is the whole request body. The repository, the entry and the
// run come from the verified JWT and the policy, never from the body.
type tokenRequest struct {
	Permissions map[string]string `json:"permissions"`
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		reply(w, 413, "request body too large")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		reply(w, 401, "missing bearer token")
		return
	}
	entry, claims, err := h.Broker.Authorize(r.Context(), token)
	if err != nil {
		var ref *Refusal
		if errors.As(err, &ref) {
			reply(w, ref.Status, ref.Reason)
			return
		}
		reply(w, 500, "internal error")
		return
	}
	if h.Mint == nil {
		reply(w, 501, "token issuance not configured")
		return
	}
	var tr tokenRequest
	if len(bytes.TrimSpace(body)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if dec.Decode(&tr) != nil {
			reply(w, 400, "invalid request body")
			return
		}
	}
	repoID, err := strconv.ParseInt(claims.RepositoryID, 10, 64)
	if err != nil {
		reply(w, 403, "repository identity mismatch")
		return
	}
	tok, err := h.Mint.Issue(r.Context(), mint.Request{
		RepositoryID: repoID, Repository: claims.Repository,
		RunID: claims.RunID, RunAttempt: claims.RunAttempt,
		WorkflowRef: claims.WorkflowRef, Event: claims.EventName, Ref: claims.Ref,
		RunnerEnvironment: claims.RunnerEnvironment, Environment: claims.Environment,
		Wanted: tr.Permissions, EntryMax: entry.Permissions,
	})
	if err != nil {
		var ref *mint.Refusal
		if errors.As(err, &ref) {
			mint.WriteRefusal(w, ref)
			return
		}
		reply(w, 500, "internal error")
		return
	}
	mint.WriteToken(w, tok)
}

func reply(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
