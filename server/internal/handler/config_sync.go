package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/configsync"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-3284: layer 1 of "merge is apply". The owner enrolls a config repo
// through the owner digest CAS (same route, same rules as workspace.context);
// a merged PR in that repo then gets a dry-run receipt comment. No code here
// writes a governed field.

// ConfigSyncGitHub is the per-repo GitHub surface of one config-sync run: the
// API reads that authorize it and the receipt comment write.
type ConfigSyncGitHub interface {
	configsync.Reader
	PostComment(ctx context.Context, number int, body string) error
}

var ownerCASWorkspaceConfigSync = ownerCASColumn{"workspace.config_sync", "config_sync", "workspace_config_sync", "settings", "workspace_id"}

// configSyncUpdate is the config_sync analogue of the other ownerCASUpdate*
// handlers. It calls ownerCASApply directly, never governedApply: no agent
// vote or decision path exists for enrollment, and a machine actor gets 403.
func (h *Handler) configSyncUpdate(w http.ResponseWriter, r *http.Request, workspaceID, actorType string, raw map[string]json.RawMessage) {
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	// The row is created lazily, and only for a caller the CAS will accept.
	if !isGovernedFieldMachineActor(r, actorType) && roleAllowed(member.Role, "owner") {
		wsUUID, _ := util.ParseUUID(workspaceID)
		if _, err := h.DB.Exec(r.Context(), `INSERT INTO workspace_config_sync (workspace_id) VALUES ($1) ON CONFLICT (workspace_id) DO NOTHING`, wsUUID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to prepare config sync settings")
			return
		}
	}
	result, ok := h.ownerCASApply(w, r, actorType, member.Role, raw, ownerCASWorkspaceConfigSync, workspaceID, workspaceID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"before_digest": result.BeforeDigest, "after_digest": result.AfterDigest})
}

// configSyncSettingsValid is the content check ownerCASApply runs for this
// column. The empty value un-enrolls.
func configSyncSettingsValid(w http.ResponseWriter, c ownerCASColumn, content string) bool {
	if c.kind != ownerCASWorkspaceConfigSync.kind || content == "" {
		return true
	}
	if _, err := configsync.ParseSettings(content); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// configSyncTargets reads live governed digests for one workspace only: every
// query carries the workspace id, so a target in another workspace is "not
// found", never readable.
type configSyncTargets struct {
	h           *Handler
	workspaceID string
}

func (c configSyncTargets) LiveDigest(ctx context.Context, target string) (string, bool, error) {
	wsUUID, err := util.ParseUUID(c.workspaceID)
	if err != nil {
		return "", false, err
	}
	var query string
	var args []any
	if target == configsync.TargetWorkspaceContext {
		col := ownerCASWorkspaceContext
		query, args = fmt.Sprintf(`SELECT %s FROM %s WHERE %s = $1`, col.digestSQL(), col.table, col.scopeCol), []any{wsUUID}
	} else if id, ok := strings.CutPrefix(target, "agent:"); ok {
		agentUUID, err := util.ParseUUID(id)
		if err != nil {
			return "", false, nil
		}
		col := ownerCASAgentInstructions
		query, args = fmt.Sprintf(`SELECT %s FROM %s WHERE id = $1 AND %s = $2`, col.digestSQL(), col.table, col.scopeCol), []any{agentUUID, wsUUID}
	} else {
		return "", false, nil
	}
	var digest string
	err = c.h.DB.QueryRow(ctx, query, args...).Scan(&digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return digest, err == nil, err
}

// configSyncDoorbell is the only part of the webhook payload this layer reads:
// which PR to ask GitHub about. Authorization never comes from it.
type configSyncDoorbell struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number int  `json:"number"`
		Merged bool `json:"merged"`
	} `json:"pull_request"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// configSyncOnPullRequest rings for every workspace that enrolled the PR's
// repo. The signature was verified by the caller. Failures are logged and
// never fail the delivery: this layer writes nothing, so a miss costs only a
// missing receipt comment.
func (h *Handler) configSyncOnPullRequest(ctx context.Context, body []byte) {
	if h.ConfigSyncGitHub == nil {
		return
	}
	var d configSyncDoorbell
	if json.Unmarshal(body, &d) != nil || d.Action != "closed" || !d.PullRequest.Merged ||
		d.Installation.ID == 0 || d.PullRequest.Number <= 0 || d.Repository.Name == "" || d.Repository.Owner.Login == "" {
		return
	}
	allowedRows, err := h.allowedGitHubInstallations(ctx, d.Installation.ID)
	if err != nil || len(allowedRows) == 0 {
		return
	}
	allowed := map[string]bool{}
	for _, row := range allowedRows {
		allowed[uuidToString(row.WorkspaceID)] = true
	}
	rows, err := h.DB.Query(ctx, `
		SELECT c.workspace_id::text, c.settings
		FROM workspace_config_sync c
		JOIN github_installation i ON i.workspace_id = c.workspace_id AND i.installation_id = $1
		WHERE c.settings <> ''`, d.Installation.ID)
	if err != nil {
		slog.Warn("config-sync: enrollment lookup failed", "err", err)
		return
	}
	type enrolled struct {
		workspaceID string
		settings    configsync.Settings
	}
	var found []enrolled
	repo := d.Repository.Owner.Login + "/" + d.Repository.Name
	for rows.Next() {
		var wsID, raw string
		if rows.Scan(&wsID, &raw) != nil || !allowed[wsID] {
			continue
		}
		s, err := configsync.ParseSettings(raw)
		if err != nil || !strings.EqualFold(s.Repo, repo) {
			continue
		}
		found = append(found, enrolled{wsID, s})
	}
	rows.Close()
	run := h.configSyncRun
	if run == nil {
		run = func(f func()) { go f() }
	}
	for _, e := range found {
		gh := h.ConfigSyncGitHub(d.Installation.ID, d.Repository.Owner.Login, d.Repository.Name)
		run(func() {
			// ponytail: a redelivery posts a second comment; the idempotency key is layer 2.
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			receipt := configsync.Evaluate(cctx, gh, configSyncTargets{h: h, workspaceID: e.workspaceID}, e.settings, d.PullRequest.Number)
			if receipt.Skipped == "skipped: not merged" || receipt.Skipped == "skipped: wrong base branch" {
				return
			}
			if err := gh.PostComment(cctx, d.PullRequest.Number, configsync.RenderComment(receipt)); err != nil {
				slog.Warn("config-sync: receipt comment failed", "workspace_id", e.workspaceID, "pr", d.PullRequest.Number, "err", err)
			}
		})
	}
}
