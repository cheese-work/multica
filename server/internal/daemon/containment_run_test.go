//go:build linux

package daemon

import (
	"context"
	"errors"
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
	d.cfg.ContainmentCharge = 2 << 30
	return d, base
}

// containmentTask is a claimed task as the server delivers it.
func containmentTask() Task {
	return Task{ID: containmentTestTask, DispatchedAt: "2026-11-23T07:00:00Z", Profile: "elevated"}
}

const containmentTestTask = "11111111-2222-3333-4444-555555555555"

// Flag off equals today's launch path: no leaf, no record, no cgroup directory,
// the prep context is the one passed in, no launch hook, PATH untouched.
func TestContainmentFlagOffIsTodaysLaunchPath(t *testing.T) {
	d, base := containmentTestDaemon(t, false)
	ctx := context.Background()
	run, outCtx, err := d.beginContainment(ctx, containmentTask())
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
	run, ctx, err := d.beginContainment(context.Background(), containmentTask())
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
	run, _, err := d.beginContainment(context.Background(), containmentTask())
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
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 126 {
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

// The persisted record carries the real claim generation, attempt generation,
// run profile and charge of the attempt, not zero values.
func TestContainmentRecordCarriesRealIdentityAndCharge(t *testing.T) {
	d, _ := containmentTestDaemon(t, true)
	task := containmentTask()
	d.inflight.begin(task.ID, "2026-11-23T07:00:05Z") // a duplicate delivery refreshed the claim
	run, _, err := d.beginContainment(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	defer run.End(slog.Default())
	rec, err := containment.ReadRecord(d.cfg.ContainmentStateDir, run.leaf.Record().Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ClaimGeneration != "2026-11-23T07:00:05Z" {
		t.Fatalf("claim generation = %q, want the current claim", rec.ClaimGeneration)
	}
	if rec.Generation != 1 || rec.LaunchProfileID != "elevated" || rec.Charge != 2<<30 {
		t.Fatalf("record = %+v", rec)
	}
	if !strings.Contains(rec.DaemonInstance, ":") || rec.BootID == "" {
		t.Fatalf("record identity = %+v", rec)
	}
	second, _, err := d.beginContainment(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	defer second.End(slog.Default())
	if g := second.leaf.Record().Generation; g != 2 {
		t.Fatalf("second attempt generation = %d, want 2", g)
	}
}

// A required value that is unavailable fails the attempt closed: no leaf.
func TestContainmentFailsClosedWithoutClaimOrCharge(t *testing.T) {
	for name, mutate := range map[string]func(*Daemon, *Task){
		"no claim generation": func(_ *Daemon, task *Task) { task.DispatchedAt = "" },
		"no charge":           func(d *Daemon, _ *Task) { d.cfg.ContainmentCharge = 0 },
	} {
		d, base := containmentTestDaemon(t, true)
		task := containmentTask()
		mutate(d, &task)
		run, _, err := d.beginContainment(context.Background(), task)
		if err == nil {
			run.End(slog.Default())
			t.Fatalf("%s: beginContainment succeeded", name)
		}
		if entries, _ := os.ReadDir(filepath.Join(base, "runs")); len(entries) != 0 {
			t.Fatalf("%s: leaf left behind: %v", name, entries)
		}
	}
}

func TestParseContainmentCharge(t *testing.T) {
	cases := []struct {
		in      string
		on      bool
		want    int64
		wantErr bool
	}{
		{"", false, 0, false},
		{"junk", false, 0, false}, // flag off ignores the knob
		{"1073741824", true, 1 << 30, false},
		{" 2048 ", true, 2048, false},
		{"", true, 0, true},
		{"0", true, 0, true},
		{"-5", true, 0, true},
		{"1GiB", true, 0, true},
	}
	for _, c := range cases {
		got, err := parseContainmentCharge(c.in, c.on)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("parseContainmentCharge(%q, %v) = %d, %v; want %d, err %v", c.in, c.on, got, err, c.want, c.wantErr)
		}
	}
}
