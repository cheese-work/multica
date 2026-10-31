package sandbox

import (
	"os"

	"github.com/multica-ai/multica/server/internal/daemon/sandbox/gateprobe"
)

// helperMain runs a helper mode of the test binary. The sandbox mounts the
// binary at bridgeMount, so a fake agent is that path plus one of these modes.
func helperMain() (code int, ok bool) {
	if len(os.Args) > 1 && os.Args[1] == BridgeHelperArg {
		return BridgeMain(os.Args[2:]), true
	}
	return gateprobe.Main(os.Args[1:])
}
