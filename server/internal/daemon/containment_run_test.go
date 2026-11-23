//go:build linux

package daemon

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/containment"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

func containmentTestDaemon(t *testing.T, on bool) (*Daemon, string) {
	t.Helper()
	base := t.TempDir()
	d := &Daemon{logger: slog.Default()}
	d.cfg.Containment = on
	d.cfg.ContainmentRoot = filepath.Join(base, "runs")
	d.cfg.ContainmentStateDir = filepath.Join(base, "state")
	return d, base
}

const containmentTestTask = "11111111-2222-3333-4444-555555555555"

// Flag off equals today's launch path: no leaf, no record, no cgroup directory,
// the prep context is the one passed in, no launch hook, PATH untouched.
func TestContainmentFlagOffIsTodaysLaunchPath(t *testing.T) {
	d, base := containmentTestDaemon(t, false)
	ctx := context.Background()
	run, outCtx, err := d.beginContainment(ctx, Task{ID: containmentTestTask})
	if err != nil {
		t.Fatal(err)
	}
	if run != nil {
		t.Fatal("flag off returned a containment run")
	}
	if outCtx != ctx || execenv.LaunchAttachFrom(outCtx) != nil {
		t.Fatal("flag off changed the prep context")
	}
	if hook := run.Hook(); hook != nil {
		t.Fatalf("flag off produced a launch hook %v", hook)
	}
	env := map[string]string{"PATH": "/usr/bin"}
	if err := run.frontShims(env, t.TempDir()); err != nil || env["PATH"] != "/usr/bin" || len(env) != 1 {
		t.Fatalf("flag off touched the env: %v %v", env, err)
	}
	run.End(slog.Default()) // nil-safe
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Fatalf("flag off created %v", entries)
	}
}

// Leaf before fork, prep in leaf: when beginContainment returns, the record is on
// disk with the cgroup ID, and the context it returns makes the prep helper
// attach to that leaf.
func TestContainmentOnCreatesLeafAndPrepAttachesToIt(t *testing.T) {
	d, _ := containmentTestDaemon(t, true)
	run, ctx, err := d.beginContainment(context.Background(), Task{ID: containmentTestTask})
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("flag on returned no containment run")
	}
	defer run.End(slog.Default())

	rec, err := containment.ReadRecord(d.cfg.ContainmentStateDir, run.leaf.Record().Attempt)
	if err != nil || rec.CgroupID == 0 || rec.State != containment.StateLeafCreated || rec.TaskID != containmentTestTask {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	attach := execenv.LaunchAttachFrom(ctx)
	if attach == nil {
		t.Fatal("prep context has no attach hook")
	}
	prep := exec.Command("true")
	if err := attach(prep); err != nil {
		t.Fatal(err)
	}
	if prep.SysProcAttr == nil || !prep.SysProcAttr.UseCgroupFD {
		t.Fatal("prep command is not forked into the leaf")
	}
	hook := run.Hook()
	if hook == nil {
		t.Fatal("no launch hook for the agent CLI")
	}
	agentCmd := exec.Command("true")
	if err := hook.Wrap(agentCmd); err != nil || agentCmd.SysProcAttr == nil || !agentCmd.SysProcAttr.UseCgroupFD {
		t.Fatalf("agent command is not forked into the leaf: %v", err)
	}
}

func TestContainmentOnFrontsBlockedLaunchShims(t *testing.T) {
	d, _ := containmentTestDaemon(t, true)
	run, _, err := d.beginContainment(context.Background(), Task{ID: containmentTestTask})
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("flag on returned no containment run")
	}
	defer run.End(slog.Default())
	env := map[string]string{"PATH": "/usr/bin"}
	if err := run.frontShims(env, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	first, rest, _ := strings.Cut(env["PATH"], string(os.PathListSeparator))
	if rest != "/usr/bin" {
		t.Fatalf("PATH = %q", env["PATH"])
	}
	out, err := exec.Command(filepath.Join(first, "docker")).CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 126 {
		t.Fatalf("docker shim: %v %s", err, out)
	}
	ev, err := containment.ReadEvents(run.leaf.EventsFile())
	if err != nil || len(ev) != 1 || ev[0].Event != "blocked_launch" || ev[0].Tool != "docker" {
		t.Fatalf("events = %+v, %v", ev, err)
	}
}

func TestParseContainmentMode(t *testing.T) {
	cases := []struct {
		in      string
		sbx     sandbox.Mode
		want    bool
		wantErr bool
	}{
		{"", sandbox.ModeOff, false, false},
		{"off", sandbox.ModeOff, false, false},
		{"on", sandbox.ModeOff, true, false},
		{"ON", sandbox.ModeAudit, true, false},
		{"maybe", sandbox.ModeOff, false, true},
		{"on", sandbox.ModeOn, false, true}, // one launch hook: sandbox enforcement owns it
	}
	for _, c := range cases {
		got, err := parseContainmentMode(c.in, c.sbx)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("parseContainmentMode(%q, %q) = %v, %v; want %v, err %v", c.in, c.sbx, got, err, c.want, c.wantErr)
		}
	}
}
