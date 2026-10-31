package handler

import (
	_ "embed"
	"net/http"
	"os"
	"regexp"
	"strings"
)

//go:embed daemon_install.sh
var daemonInstallScript string

const daemonInstallURLPlaceholder = "__MULTICA_SERVER_URL__"

// safeServerURL is what may be written into the script's single-quoted
// literal: scheme, host[:port], optional plain path. Nothing the shell would
// interpret.
var safeServerURL = regexp.MustCompile(`^https?://(\[[0-9a-fA-F:]+\]|[A-Za-z0-9.-]+)(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)

// daemonInstallServerURL picks the URL the script downloads from: the
// operator's configured public URL, else the URL this request arrived on.
func daemonInstallServerURL(r *http.Request) (string, bool) {
	for _, env := range []string{"MULTICA_DAEMON_SERVER_URL", "MULTICA_PUBLIC_URL"} {
		if u := normalizePublicURL(os.Getenv(env)); safeServerURL.MatchString(u) {
			return u, true
		}
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https") {
		scheme = "https"
	}
	u := scheme + "://" + r.Host
	return u, safeServerURL.MatchString(u)
}

// DaemonInstallScript serves GET /api/daemon/install.sh.
//
// It is public on purpose: `curl -fsSL <server>/api/daemon/install.sh | bash`
// runs on a host that has no token yet. The script is static and carries no
// secret; the binary and manifest endpoints it calls stay behind DaemonAuth.
func DaemonInstallScript(w http.ResponseWriter, r *http.Request) {
	serverURL, ok := daemonInstallServerURL(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "cannot determine a safe server URL; set MULTICA_PUBLIC_URL")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The body depends on the Host header; never let a shared cache replay it.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(strings.ReplaceAll(daemonInstallScript, daemonInstallURLPlaceholder, serverURL)))
}
