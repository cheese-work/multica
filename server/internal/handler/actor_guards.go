package handler

import (
	"net/http"
	"regexp"

	"github.com/multica-ai/multica/server/internal/dispatch"
)

// RequireHumanActor rejects machine credentials on human-only route groups.
// Auth strips client-supplied X-Actor-Source and stamps task_token or cloud_pat;
// that authoritative header distinguishes a machine acting for its human owner.
// resolveActor classifies workspace authorship and is not this authorization gate.
//
// Attach this middleware only to human-only scopes such as billing. Issue and
// chat routes permit authorized agent traffic. Classify new auth sources here
// when adding them; unknown values currently remain human-equivalent.
func RequireHumanActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMachineCredentialActor(r) {
			writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireHumanOrTaskToken is RequireHumanActor except that an agent task token
// passes. It is for routes whose handler authorizes the agent itself, such as
// a decision-bound write (CHE-2456); every other machine credential stays 403.
func RequireHumanOrTaskToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMachineCredentialActor(r) && r.Header.Get("X-Actor-Source") != "task_token" {
			writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isMachineCredentialActor centralizes the authoritative actor-source check so
// sensitive handlers can keep a fail-closed backstop in addition to their
// router middleware. Unknown actor sources intentionally remain human-equivalent
// until their authentication branch is explicitly classified here.
func isMachineCredentialActor(r *http.Request) bool {
	// X-Actor-Source is server-set only. The auth middleware strips any
	// client-supplied value before stamping its own, so a recognized value here
	// is authoritative.
	switch r.Header.Get("X-Actor-Source") {
	case "task_token", "cloud_pat":
		return true
	default:
		return false
	}
}

// recoveryWritable lists the only writes a budget recovery run may make: record
// why the issue is blocked (comment, summary, status move, attachment). Any other
// write is refused, so an enqueue path nobody listed cannot start work for it.
var recoveryWritable = regexp.MustCompile(`^(PUT /api/issues/[^/]+|POST /api/issues/[^/]+/(move|comments)|(PUT|DELETE) /api/issues/[^/]+/metadata/[^/]+|PUT /api/comments/[^/]+|(POST|DELETE) /api/comments/[^/]+/resolve|POST /api/upload-file)$`)

// RejectRecoveryWrites is deny-by-default for a budget recovery run's task token
// (CHE-2297 L3): reads pass, writes pass only if recoveryWritable names them.
func RejectRecoveryWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dispatch.BudgetRecoveryCaller(r.Context()) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
			default:
				if !recoveryWritable.MatchString(r.Method + " " + r.URL.Path) {
					writeError(w, http.StatusForbidden, errRecoveryCaused)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
