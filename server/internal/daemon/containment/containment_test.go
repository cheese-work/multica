//go:build linux

package containment

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	return Options{
		Root:            filepath.Join(root, "runs"),
		StateDir:        filepath.Join(root, "state"),
		BootID:          "boot-1",
		DaemonInstance:  "inst-1",
		TaskID:          "11111111-2222-3333-4444-555555555555",
		ClaimGeneration: 3,
		Generation:      1,
		LaunchProfileID: "profile-a",
	}
}

func dirInode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// Leaf before fork: when Create returns, the leaf directory exists and the
// fsynced attempt record already carries its cgroup ID, before any process is
// built or started (plan r3 3.2 item 1).
func TestCreateWritesRecordWithCgroupIDBeforeAnyFork(t *testing.T) {
	o := testOptions(t)
	l, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}
	defer l.End()

	got, err := ReadRecord(o.StateDir, l.Record().Attempt)
	if err != nil {
		t.Fatalf("record not on disk right after Create: %v", err)
	}
	if got.State != StateLeafCreated {
		t.Fatalf("state = %q, want %q", got.State, StateLeafCreated)
	}
	if got.CgroupID == 0 || got.CgroupID != dirInode(t, got.LeafPath) {
		t.Fatalf("cgroup id = %d, leaf inode = %d", got.CgroupID, dirInode(t, got.LeafPath))
	}
	if !strings.HasPrefix(got.LeafPath, o.Root+string(os.PathSeparator)) {
		t.Fatalf("leaf %q is not under root %q", got.LeafPath, o.Root)
	}
}

func TestRecordCarriesEveryPlanField(t *testing.T) {
	o := testOptions(t)
	l, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}
	defer l.End()
	r, err := ReadRecord(o.StateDir, l.Record().Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if r.BootID != "boot-1" || r.DaemonInstance != "inst-1" || r.TaskID != o.TaskID ||
		r.ClaimGeneration != 3 || r.Generation != 1 || r.LaunchProfileID != "profile-a" ||
		r.Attempt == "" || r.LeafPath == "" || r.CgroupID == 0 || r.State != StateLeafCreated {
		t.Fatalf("record is missing a field: %+v", r)
	}
	if !strings.HasPrefix(r.Attempt, o.TaskID+"-") {
		t.Fatalf("attempt %q does not name the task", r.Attempt)
	}
}

// The spawn uses CLONE_INTO_CGROUP on the leaf's directory fd.
func TestAttachUsesCloneIntoCgroupOnTheLeaf(t *testing.T) {
	l, err := Create(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer l.End()
	cmd := exec.Command("true")
	if err := l.Attach(cmd); err != nil {
		t.Fatal(err)
	}
	a := cmd.SysProcAttr
	if a == nil || !a.UseCgroupFD || a.CgroupFD <= 0 {
		t.Fatalf("SysProcAttr = %+v, want UseCgroupFD with a leaf fd", a)
	}
	var fst, lst syscall.Stat_t
	if err := syscall.Fstat(a.CgroupFD, &fst); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(l.Record().LeafPath, &lst); err != nil {
		t.Fatal(err)
	}
	if fst.Ino != lst.Ino {
		t.Fatalf("cgroup fd inode %d is not the leaf inode %d", fst.Ino, lst.Ino)
	}
}

// Wrap is the agent.LaunchHook method; it must do what Attach does.
func TestWrapIsAttach(t *testing.T) {
	l, err := Create(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer l.End()
	cmd := exec.Command("true")
	if err := l.Wrap(cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.UseCgroupFD {
		t.Fatal("Wrap did not attach the leaf")
	}
}

func TestAttachAfterEndFails(t *testing.T) {
	l, err := Create(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.End(); err != nil {
		t.Fatal(err)
	}
	if err := l.Attach(exec.Command("true")); err == nil {
		t.Fatal("attach to an ended leaf must fail")
	}
}

func TestEndRemovesLeafAndMarksRecord(t *testing.T) {
	o := testOptions(t)
	l, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}
	rec := l.Record()
	if err := l.End(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rec.LeafPath); !os.IsNotExist(err) {
		t.Fatalf("leaf still present: %v", err)
	}
	got, err := ReadRecord(o.StateDir, rec.Attempt)
	if err != nil || got.State != StateEnded {
		t.Fatalf("record = %+v, %v; want state %q", got, err, StateEnded)
	}
	if err := l.End(); err != nil {
		t.Fatalf("second End: %v", err)
	}
}

func TestCreateRejectsUnsafeTaskID(t *testing.T) {
	for _, id := range []string{"", "../x", "a/b", "a b"} {
		o := testOptions(t)
		o.TaskID = id
		if _, err := Create(o); err == nil {
			t.Fatalf("task id %q accepted", id)
		}
	}
}

// A failed record write must not leave a leaf behind.
func TestCreateCleansUpWhenRecordCannotBeWritten(t *testing.T) {
	o := testOptions(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	o.StateDir = filepath.Join(blocker, "state") // parent is a file
	if _, err := Create(o); err == nil {
		t.Fatal("expected an error")
	}
	if entries, _ := os.ReadDir(o.Root); len(entries) != 0 {
		t.Fatalf("leaf left behind: %v", entries)
	}
}

func TestWriteRecordIsAtomicAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	r := Record{Attempt: "a-1", State: StateLeafCreated, CgroupID: 7}
	if err := WriteRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	r.State = StateEnded
	if err := WriteRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("state dir = %v, want only the record", entries)
	}
	got, err := ReadRecord(dir, "a-1")
	if err != nil || got.State != StateEnded {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// Shims: each blocked tool exits 126 and appends a blocked_launch event.
func TestShimsExit126AndRecordBlockedLaunch(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	bin := filepath.Join(dir, "bin")
	if err := InstallShims(bin, events); err != nil {
		t.Fatal(err)
	}
	for _, name := range ShimNames {
		cmd := exec.Command(filepath.Join(bin, name), "arg")
		out, err := cmd.CombinedOutput()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 126 {
			t.Fatalf("%s: err = %v, want exit 126", name, err)
		}
		if !strings.Contains(string(out), name) {
			t.Fatalf("%s: message %q does not name the tool", name, out)
		}
	}
	got, err := ReadEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ShimNames) {
		t.Fatalf("events = %v, want one per tool", got)
	}
	for i, e := range got {
		if e.Event != "blocked_launch" || e.Tool != ShimNames[i] {
			t.Fatalf("event %d = %+v", i, e)
		}
	}
}

func TestShimNamesAreThePlanList(t *testing.T) {
	want := []string{"docker", "docker-compose", "podman", "systemd-run", "tmux", "screen", "ssh"}
	if strings.Join(ShimNames, ",") != strings.Join(want, ",") {
		t.Fatalf("ShimNames = %v, want %v", ShimNames, want)
	}
}

func TestInstallShimsRejectsUnsafePaths(t *testing.T) {
	if err := InstallShims(t.TempDir(), "/tmp/it's"); err == nil {
		t.Fatal("events path with a quote accepted")
	}
}
