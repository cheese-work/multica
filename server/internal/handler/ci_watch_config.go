package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/ciwatch"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CI watcher settings and event log (CHE-4281). Human owner/admin only; the
// whole surface answers "disabled on this server" while the ci_watcher flag is off.

const (
	ciWatchBodyLimit    = 16 << 10
	ciWatchDefaultLimit = 50
	ciWatchMaxLimit     = 200
)

type ciWatchRepo struct{ workspaceID, owner, name string }

// ciWatchAdmin runs the checks every endpoint shares and returns the target repo.
func (h *Handler) ciWatchAdmin(w http.ResponseWriter, r *http.Request) (ciWatchRepo, bool) {
	id := workspaceIDFromURL(r, "id")
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return ciWatchRepo{}, false
	}
	if _, ok := h.requireWorkspaceRole(w, r, id, "workspace not found", "owner", "admin"); !ok {
		return ciWatchRepo{}, false
	}
	if !featureflags.CIWatcherEnabled(r.Context(), h.FeatureFlags) {
		writeFeatureDisabled(w, "ci_watcher_disabled", "CI watcher is disabled on this server")
		return ciWatchRepo{}, false
	}
	raw := chi.URLParam(r, "repo")
	if unescaped, err := url.PathUnescape(raw); err == nil {
		raw = unescaped
	}
	owner, name, err := ciwatch.ParseRepo(raw)
	if err != nil {
		writeCIWatchValidation(w, err)
		return ciWatchRepo{}, false
	}
	return ciWatchRepo{workspaceID: id, owner: owner, name: name}, true
}

func writeCIWatchValidation(w http.ResponseWriter, err error) {
	var ve *ciwatch.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ve.Error(), "field": ve.Field})
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func (h *Handler) GetCIWatchConfig(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.ciWatchAdmin(w, r)
	if !ok {
		return
	}
	cfg, err := ciwatch.GetConfig(r.Context(), h.DB, repo.workspaceID, repo.owner, repo.name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load CI watch config")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (h *Handler) PutCIWatchConfig(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.ciWatchAdmin(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, ciWatchBodyLimit))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON object")
		return
	}
	patch, err := ciwatch.ParsePatch(raw)
	if err != nil {
		writeCIWatchValidation(w, err)
		return
	}
	if err := h.checkCIWatchTargets(r, repo.workspaceID, patch); err != nil {
		var ve *ciwatch.ValidationError
		if errors.As(err, &ve) {
			writeCIWatchValidation(w, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to check CI watch targets")
		return
	}
	res, err := ciwatch.ApplyConfig(r.Context(), h.TxStarter, repo.workspaceID, repo.owner, repo.name, requestUserID(r), patch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save CI watch config")
		return
	}
	if res.Changed == nil {
		res.Changed = []string{}
	}
	writeJSON(w, http.StatusOK, res)
}

// checkCIWatchTargets proves the fix owner and the notified member exist in this
// workspace, so the watcher never assigns work to, or notifies, a stranger.
func (h *Handler) checkCIWatchTargets(r *http.Request, workspaceID string, patch ciwatch.Patch) error {
	ws, err := util.ParseUUID(workspaceID)
	if err != nil {
		return err
	}
	check := func(field string, lookup func(id string) error) error {
		v, _ := patch.Get(field)
		id, _ := v.(string)
		if id == "" {
			return nil
		}
		err := lookup(id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &ciwatch.ValidationError{Field: field, Msg: "not found in this workspace"}
		}
		return err
	}
	ctx := r.Context()
	if err := check("fix_owner_id", func(id string) error {
		uid, err := util.ParseUUID(id)
		if err != nil {
			return err
		}
		if kind, _ := patch.Get("fix_owner_kind"); kind == "squad" {
			_, err = h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: uid, WorkspaceID: ws})
		} else {
			_, err = h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: uid, WorkspaceID: ws})
		}
		return err
	}); err != nil {
		return err
	}
	return check("notify_member_id", func(id string) error {
		uid, err := util.ParseUUID(id)
		if err != nil {
			return err
		}
		_, err = h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: uid, WorkspaceID: ws})
		return err
	})
}

func (h *Handler) ListCIWatchEvents(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.ciWatchAdmin(w, r)
	if !ok {
		return
	}
	limit := ciWatchDefaultLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > ciWatchMaxLimit {
			writeError(w, http.StatusBadRequest, "limit must be an integer from 1 to "+strconv.Itoa(ciWatchMaxLimit))
			return
		}
		limit = n
	}
	events, err := ciwatch.ListEvents(r.Context(), h.DB, repo.workspaceID, repo.owner, repo.name, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list CI watch events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}
