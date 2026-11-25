package containment

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ShimNames are the tools a contained run cannot start through PATH (plan r3
// 3.3 item 3). A call by absolute path bypasses a shim; that stays a named
// residual, not coverage.
var ShimNames = []string{"docker", "docker-compose", "podman", "systemd-run", "tmux", "screen", "ssh"}

// Event is one line of the attempt's event log.
type Event struct {
	Event string `json:"event"`
	Tool  string `json:"tool"`
	Time  string `json:"time"`
}

// InstallShims writes one shim per ShimNames into dir. Each shim exits 126 with
// a message and appends a blocked_launch event to eventsFile. Put dir first on
// the run's PATH.
func InstallShims(dir, eventsFile string) error {
	if strings.ContainsAny(eventsFile, "'\n") || strings.ContainsAny(dir, "\n") {
		return errors.New("containment: shim paths must not contain a quote or newline")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("containment: shim dir: %w", err)
	}
	for _, name := range ShimNames {
		script := fmt.Sprintf(`#!/bin/sh
printf '{"event":"blocked_launch","tool":"%[1]s","time":"%%s"}\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" >> '%[2]s' 2>/dev/null
echo "multica containment: %[1]s is blocked in a contained run" >&2
exit 126
`, name, eventsFile)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			return fmt.Errorf("containment: shim %s: %w", name, err)
		}
	}
	return nil
}

// ReadEvents reads an attempt's event log; a missing file has no events.
func ReadEvents(path string) ([]Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("containment: event line %q: %w", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
