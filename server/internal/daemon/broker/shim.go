package broker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteShim writes a `git` wrapper into dir and returns its path. `git push`
// becomes `multica git push`; every other command runs the real git. A push
// that dodges the wrapper (`git -c x=y push`) reaches real git with no
// credential and fails; the wrapper is a convenience, not the control.
func WriteShim(dir, realGit, multica string) (string, error) {
	for _, p := range []string{realGit, multica} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "'\n") {
			return "", fmt.Errorf("broker shim: %q is not a plain absolute path", p)
		}
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = push ]; then exec '%s' git \"$@\"; fi\nexec '%s' \"$@\"\n", multica, realGit)
	path := filepath.Join(dir, "git")
	return path, os.WriteFile(path, []byte(script), 0o755)
}

// WriteGHShim writes a `gh` wrapper into dir and returns its path. Each call
// asks `multica git read-token` for the run's read token, which refreshes it
// through the broker when it is close to expiry, and runs the real gh with only
// that token. With no token gh does not run: it never falls back to an ambient
// credential.
func WriteGHShim(dir, realGH, multica string) (string, error) {
	for _, p := range []string{realGH, multica} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "'\n") {
			return "", fmt.Errorf("broker shim: %q is not a plain absolute path", p)
		}
	}
	script := fmt.Sprintf("#!/bin/sh\nt=$('%s' git read-token) || exit 1\n[ -n \"$t\" ] || exit 1\n"+
		"unset GITHUB_TOKEN GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN\nGH_TOKEN=$t\nexport GH_TOKEN\nexec '%s' \"$@\"\n", multica, realGH)
	path := filepath.Join(dir, "gh")
	return path, os.WriteFile(path, []byte(script), 0o755)
}
