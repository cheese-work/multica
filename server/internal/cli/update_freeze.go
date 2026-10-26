package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// UpdateFreezeFileEnv overrides the freeze file path (tests, non-default homes).
const UpdateFreezeFileEnv = "MULTICA_UPDATE_FREEZE_FILE"

// CheckUpdateFreeze returns an error while the freeze file exists. An operator
// creates it (contents = the reason, e.g. the pending owner packet) to stop
// every CLI/daemon self-update and removes it when the packet closes. Default
// path: ~/.multica/UPDATE_FREEZE. Presence is the whole contract — a file that
// cannot be read, or whose default path cannot be resolved, also freezes (fail closed).
func CheckUpdateFreeze() error {
	path := strings.TrimSpace(os.Getenv(UpdateFreezeFileEnv))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("updates frozen: cannot locate the freeze file (no home dir; set %s): %w", UpdateFreezeFileEnv, err)
		}
		path = filepath.Join(home, ".multica", "UPDATE_FREEZE")
	}
	reason, err := os.ReadFile(path)
	switch {
	case err == nil:
		return fmt.Errorf("updates frozen by %s (%s); remove it when the owner packet closes", path, strings.TrimSpace(string(reason)))
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("updates frozen: cannot read %s: %w", path, err)
	}
}
