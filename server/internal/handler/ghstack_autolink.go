package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/ghstack"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-4165 (CHE-4143 step 5): read and write API behind
// `multica github stack-autolink status|set`. The setting itself, its
// validation and the resolver live in integrations/ghstack (step 4); this file
// only exposes them.

// stackAutolinkRepo normalizes "owner/repo" to lowercase, the key the setting
// uses. It reports false for anything else.
func stackAutolinkRepo(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(s, " ?#") || strings.Contains(name, "/") {
		return "", false
	}
	return s, true
}

type stackAutolinkWorkspace struct {
	WorkspaceID   string `json:"workspace_id"`
	Name          string `json:"name"`
	EffectiveMode string `json:"effective_mode"`
}

type stackAutolinkStatusResponse struct {
	Repo      string `json:"repo"`
	FinalMode string `json:"final_mode"`
	EnvKill   bool   `json:"env_kill"`
	// Workspaces are the consulted workspaces the caller belongs to. Others
	// count in the final mode but are only counted here, never named.
	Workspaces       []stackAutolinkWorkspace `json:"workspaces"`
	HiddenWorkspaces int                      `json:"hidden_workspaces"`
}

// GetGitHubStackAutolink (GET /api/github/stack-autolink?repo=owner/repo)
// prints the final mode for the repo and, per consulted workspace the caller
// belongs to, its effective mode. Consulted = every workspace bound to a
// GitHub App installation of the repo owner that the caller's workspace has
// bound (plan, Effective mode rule 2). Read-only.
func (h *Handler) GetGitHubStackAutolink(w http.ResponseWriter, r *http.Request) {
	repo, ok := stackAutolinkRepo(r.URL.Query().Get("repo"))
	if !ok {
		writeError(w, http.StatusBadRequest, "repo must be owner/repo")
		return
	}
	wsID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, wsID, "workspace id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceMember(w, r, wsID, "workspace not found"); !ok {
		return
	}
	ctx := r.Context()
	owner, _, _ := strings.Cut(repo, "/")

	own, err := h.Queries.ListGitHubInstallationsByWorkspace(ctx, wsUUID)
	if err != nil {
		slog.Error("stack autolink status: list installations", "workspace_id", wsID, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read installations")
		return
	}
	bound := map[string]db.Workspace{}
	for _, inst := range own {
		if !strings.EqualFold(inst.AccountLogin, owner) {
			continue
		}
		rows, err := h.Queries.ListGitHubInstallationsByInstallationID(ctx, inst.InstallationID)
		if err != nil {
			slog.Error("stack autolink status: list bindings", "workspace_id", wsID, "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read installation bindings")
			return
		}
		for _, row := range rows {
			id := uuidToString(row.WorkspaceID)
			if _, seen := bound[id]; seen {
				continue
			}
			ws, err := h.Queries.GetWorkspace(ctx, row.WorkspaceID)
			if err != nil {
				slog.Error("stack autolink status: read workspace", "workspace_id", id, "err", err)
				writeError(w, http.StatusInternalServerError, "failed to read a bound workspace")
				return
			}
			bound[id] = ws
		}
	}

	userID := requestUserID(r)
	resp := stackAutolinkStatusResponse{Repo: repo, EnvKill: ghstack.EnvKill(), Workspaces: []stackAutolinkWorkspace{}}
	settings := make([][]byte, 0, len(bound))
	for id, ws := range bound {
		settings = append(settings, ws.Settings)
		if _, err := h.getWorkspaceMember(ctx, userID, id); err != nil {
			resp.HiddenWorkspaces++
			continue
		}
		resp.Workspaces = append(resp.Workspaces, stackAutolinkWorkspace{
			WorkspaceID: id, Name: ws.Name, EffectiveMode: string(ghstack.EffectiveMode(repo, ws.Settings)),
		})
	}
	sort.Slice(resp.Workspaces, func(i, j int) bool { return resp.Workspaces[i].WorkspaceID < resp.Workspaces[j].WorkspaceID })
	resp.FinalMode = string(ghstack.FinalMode(repo, settings, resp.EnvKill))
	writeJSON(w, http.StatusOK, resp)
}

type stackAutolinkSetResponse struct {
	WorkspaceID string `json:"workspace_id"`
	Repo        string `json:"repo,omitempty"`
	Mode        string `json:"mode"`
	// EffectiveMode is this workspace's mode for the repo after the write
	// (empty without a repo): a workspace "off" still beats a repo entry.
	EffectiveMode string `json:"effective_mode,omitempty"`
}

// SetGitHubStackAutolink (PUT /api/github/stack-autolink) sets the workspace
// mode, or with "repo" one repo's mode, in the workspace settings key. Workspace
// owner or admin only. Other settings keys are kept.
func (h *Handler) SetGitHubStackAutolink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo string `json:"repo"`
		Mode any    `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	mode, _ := req.Mode.(string)
	switch ghstack.Mode(mode) {
	case ghstack.ModeOff, ghstack.ModeReport, ghstack.ModeLink:
	default:
		writeError(w, http.StatusBadRequest, "mode must be off, report or link")
		return
	}
	repo := ""
	if strings.TrimSpace(req.Repo) != "" {
		var ok bool
		if repo, ok = stackAutolinkRepo(req.Repo); !ok {
			writeError(w, http.StatusBadRequest, "repo must be owner/repo")
			return
		}
	}
	wsID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, wsID, "workspace id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceRole(w, r, wsID, "workspace not found", "owner", "admin"); !ok {
		return
	}

	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update the setting")
		return
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	// The row lock makes the read-modify-write safe against another settings write.
	storedJSON, err := q.LockWorkspaceSettings(ctx, wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update the setting")
		return
	}
	before, err := q.GetWorkspace(ctx, wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update the setting")
		return
	}
	settings := map[string]any{}
	if err := json.Unmarshal(storedJSON, &settings); err != nil || settings == nil {
		settings = map[string]any{}
	}
	cfg, _ := settings[ghstack.SettingKey].(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
	}
	if repo == "" {
		cfg["mode"] = mode
	} else {
		repos, _ := cfg["repos"].(map[string]any)
		if repos == nil {
			repos = map[string]any{}
		}
		for k := range repos {
			if strings.EqualFold(k, repo) {
				delete(repos, k) // replace a case variant, do not duplicate it
			}
		}
		repos[repo] = mode
		cfg["repos"] = repos
	}
	settings[ghstack.SettingKey] = cfg
	if err := ghstack.ValidateSetting(settings); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	newSettings, _ := json.Marshal(settings)
	ws, err := q.UpdateWorkspace(ctx, db.UpdateWorkspaceParams{ID: wsUUID, Settings: newSettings})
	if err != nil {
		slog.Warn("stack autolink set failed", "workspace_id", wsID, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to update the setting")
		return
	}
	if err := h.writeAudit(ctx, q, r, auditInput{
		Kind: "workspace.update", WorkspaceID: ws.ID, TargetID: ws.ID,
		Before: workspaceAuditFields(before), After: workspaceAuditFields(ws),
	}); err != nil {
		if failedAudit(w, r, auditWriteError{err}, "workspace.update") {
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update the setting")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Warn("stack autolink set commit failed", "workspace_id", wsID, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to update the setting")
		return
	}

	userID := requestUserID(r)
	h.publish(protocol.EventWorkspaceUpdated, uuidToString(ws.ID), "member", userID, map[string]any{"workspace": h.workspaceToResponse(ws)})
	// Daemons cache workspace settings and learn of a change only this way.
	if members, err := h.Queries.ListMembers(ctx, ws.ID); err == nil {
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, uuidToString(m.UserID))
		}
		h.notifyDaemonWorkspacesChanged(ids...)
	}
	resp := stackAutolinkSetResponse{WorkspaceID: wsID, Repo: repo, Mode: mode}
	if repo != "" {
		resp.EffectiveMode = string(ghstack.EffectiveMode(repo, ws.Settings))
	}
	writeJSON(w, http.StatusOK, resp)
}
