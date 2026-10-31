package handler

import (
	"encoding/json"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Reasons a runtime gets no update verdict. Empty means the verdict is firm
// (update available, or already on the server build).
const (
	updateReasonUnknownCommit = "unknown_commit"
	updateReasonNoBuild       = "no_build"
	updateReasonUnsupportedOS = "unsupported_os"
)

// RuntimeUpdateStatus is what the Runtimes page needs to decide whether to
// offer a daemon update: whether the server's build differs from the one the
// runtime reports, and why not when it cannot say.
type RuntimeUpdateStatus struct {
	UpdateAvailable bool
	ServerCommit    string
	ServerVersion   string
	Reason          string
}

// runtimeUpdateStatus compares the build a runtime registered with against
// this server's daemon build manifest. It mirrors cli.ResolveForkUpdate (the
// path the daemon actually takes) so the badge and the update agree; a nil
// receiver means the server ships no daemon build.
func (s *DaemonUpdateSource) runtimeUpdateStatus(metadata []byte) RuntimeUpdateStatus {
	if s == nil {
		return RuntimeUpdateStatus{Reason: updateReasonNoBuild}
	}
	st := RuntimeUpdateStatus{ServerCommit: s.manifest.Commit, ServerVersion: s.manifest.Version}

	var meta struct {
		Commit string `json:"cli_commit"`
		OS     string `json:"cli_os"`
		Arch   string `json:"cli_arch"`
	}
	if len(metadata) > 0 {
		_ = json.Unmarshal(metadata, &meta) // malformed metadata reads as unknown
	}

	switch {
	case !cli.ValidCommit(meta.Commit) || !cli.ValidCommit(s.manifest.Commit):
		st.Reason = updateReasonUnknownCommit
	case !cli.ForkUpdateSupported(meta.OS):
		st.Reason = updateReasonUnsupportedOS
	default:
		if _, ok := s.manifest.Binaries[meta.OS+"-"+meta.Arch]; !ok {
			st.Reason = updateReasonNoBuild
		} else {
			st.UpdateAvailable = !strings.HasPrefix(s.manifest.Commit, meta.Commit)
		}
	}
	return st
}

// runtimeResponse is runtimeToResponse plus the per-runtime update verdict.
func (h *Handler) runtimeResponse(rt db.AgentRuntime) AgentRuntimeResponse {
	resp := runtimeToResponse(rt)
	st := h.DaemonUpdate.runtimeUpdateStatus(rt.Metadata)
	resp.UpdateAvailable = st.UpdateAvailable
	resp.ServerCommit = st.ServerCommit
	resp.ServerVersion = st.ServerVersion
	resp.UpdateReason = st.Reason
	return resp
}
