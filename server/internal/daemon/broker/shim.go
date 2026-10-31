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
