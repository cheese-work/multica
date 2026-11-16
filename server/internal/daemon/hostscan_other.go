//go:build !linux

package daemon

// scanHostTaskIDs: the host scan reads /proc, so elsewhere it never proves
// anything and the server keeps the uptime rule.
func scanHostTaskIDs() ([]string, bool) { return nil, false }
