//go:build !linux

package daemon

import (
	"errors"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// newProcHostCollector: host sampling is Linux only; elsewhere no sample is
// ever produced and heartbeats carry none.
func newProcHostCollector() func() (*protocol.DaemonHostSample, error) {
	return func() (*protocol.DaemonHostSample, error) { return nil, errors.New("host sampling is Linux only") }
}
