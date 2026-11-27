package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/gatesuite"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// X2 (CHE-10369, plan RSC-1 R4): run A's broker socket is not reachable from
// run B's sandbox. Both runs get a real broker from applyRunBroker; run B's
// spec is built the way runTask builds it and rendered to a bwrap argv.
func TestRunSocketNotInOtherRunSandbox(t *testing.T) {
	fx := newSpecFixture(t)
	t.Setenv("TMPDIR", t.TempDir()) // both brokers' directories now share one parent
	gh := gatesuite.NewGitHub(t, gatesuite.GitHubOptions{Repos: map[string]int64{"o/r": 42}})
	cfg := brokerTestConfig(t, gh)
	brokerFakeTools(t)
	multicaExe = func() (string, error) { return os.Args[0], nil }
	t.Cleanup(func() { multicaExe = os.Executable })

	start := func() *runBroker {
		rb, err := applyRunBroker(sandbox.ModeOn, true, cfg, map[string]string{}, quietLog())
		if err != nil || rb == nil {
			t.Fatalf("applyRunBroker: %v, %v", rb, err)
		}
		t.Cleanup(rb.Close)
		return rb
	}
	runA, runB := start(), start()
	if filepath.Dir(runA.Socket) == filepath.Dir(runB.Socket) {
		t.Fatal("two runs share one broker socket directory")
	}

	in := fx.in
	in.BrokerSocket, in.BrokerShimDir = runB.Socket, runB.ShimDir
	spec, _ := buildRunSandboxSpec(in)
	spec.Argv, spec.NoDaemonAPI = []string{"/bin/true"}, true
	argv, err := (&sandbox.Launcher{Mode: sandbox.ModeOn, BridgeExe: os.Args[0]}).Args(spec)
	if err != nil {
		t.Fatal(err)
	}

	for _, a := range argv { // no bind may be, or contain, run A's socket
		if filepath.IsAbs(a) && (runA.Socket == a || strings.HasPrefix(runA.Socket, strings.TrimSuffix(a, "/")+"/")) && a != "/" {
			t.Fatalf("run A's broker socket is reachable from run B's sandbox through bind %q", a)
		}
	}
	if !strings.Contains(strings.Join(argv, "\x00"), filepath.Dir(runB.Socket)) {
		t.Fatal("run B's own broker socket directory is not bound")
	}
	for _, kv := range spec.Env {
		if strings.Contains(kv, runA.Socket) {
			t.Fatal("run A's socket path is in run B's env")
		}
	}
}
