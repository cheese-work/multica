package agent

import (
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

// restrictedEnv is the runtime-wide switch (CHE-2216 slice A2b). Off keeps the
// daemon's whole environment flowing into agent children, as before.
var restrictedEnv atomic.Bool

// SetRestrictedEnv turns the inherited-environment allowlist on or off. The
// daemon sets it once at startup from the sandbox enforcement mode.
func SetRestrictedEnv(on bool) { restrictedEnv.Store(on) }

// BaseEnviron is the environment an agent child inherits from the daemon:
// all of os.Environ(), or only the allowlisted names when restricted.
func BaseEnviron() []string {
	if !restrictedEnv.Load() {
		return os.Environ()
	}
	kept, _ := SplitEnv(os.Environ())
	return kept
}

// restrictedEnvNames are the only inherited names a restricted run keeps.
// HOME is absent on purpose: the daemon sets the per-run HOME explicitly.
// Daemon-held credentials (GH_TOKEN, ANTHROPIC_AUTH_TOKEN, PRIVATE_KEY_PATH,
// SSH_AUTH_SOCK, ...) are absent by construction. Task variables and
// per-agent custom_env reach the child through the explicit extras, not here.
var restrictedEnvNames = map[string]bool{
	"PATH": true, "LANG": true, "LANGUAGE": true, "TERM": true, "TZ": true,
	"USER": true, "LOGNAME": true, "SHELL": true, "COLORTERM": true, "NO_COLOR": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	// Read-only toolchain locations.
	"JAVA_HOME": true, "ANDROID_HOME": true, "ANDROID_SDK_ROOT": true, "GOROOT": true,
	// Provider endpoint and model selection; never a credential.
	"ANTHROPIC_BASE_URL": true, "ANTHROPIC_MODEL": true, "OPENAI_BASE_URL": true,
}

// SplitEnv partitions environ into the entries a restricted run keeps and the
// names it drops. Dropped values are never returned, so callers can log them.
func SplitEnv(environ []string) (kept, droppedNames []string) {
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if restrictedEnvNames[key] || strings.HasPrefix(key, "LC_") {
			kept = append(kept, entry)
		} else {
			droppedNames = append(droppedNames, key)
		}
	}
	sort.Strings(droppedNames)
	return kept, droppedNames
}
