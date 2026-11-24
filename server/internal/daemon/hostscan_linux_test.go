//go:build linux

package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func fakeProc(t *testing.T, environs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, env := range environs {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
		fakeProcMeta(t, dir, os.Getuid(), "0::/daemon.service")
	}
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "self", "cgroup"), []byte("0::/daemon.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// fakeProcMeta writes the world-readable files the scan trusts instead of
// environ: status (owner uid) and cgroup.
func fakeProcMeta(t *testing.T, dir string, uid int, cgroup string) {
	t.Helper()
	status := fmt.Sprintf("Name:\tx\nUid:\t%d\t%d\t%d\t%d\n", uid, uid, uid, uid)
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// CHE-5745: a runner carries MULTICA_TASK_ID, so the host scan can prove a
// task absent without trusting daemon uptime.
func TestScanProcTaskIDsFindsRunnerEnviron(t *testing.T) {
	root := fakeProc(t, map[string]string{
		"101": "PATH=/bin\x00MULTICA_TASK_ID=task-a\x00HOME=/h\x00",
		"102": "PATH=/bin\x00",
		"103": "MULTICA_TASK_ID=task-b\x00",
	})
	if err := os.MkdirAll(filepath.Join(root, "self-not-a-pid"), 0o755); err != nil {
		t.Fatal(err)
	}
	ids, ok := scanProcTaskIDs(root)
	if !ok {
		t.Fatal("scan not ok")
	}
	sort.Strings(ids)
	if want := []string{"task-a", "task-b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

// Any error reading a same-uid process proves nothing: scan not ok.
func TestScanProcTaskIDsErrorIsNotOK(t *testing.T) {
	root := fakeProc(t, map[string]string{"101": "MULTICA_TASK_ID=task-a\x00"})
	// environ as a directory: ReadFile fails with EISDIR, even for root.
	bad := filepath.Join(root, "102", "environ")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeProcMeta(t, filepath.Join(root, "102"), os.Getuid(), "0::/daemon.service")
	if _, ok := scanProcTaskIDs(root); ok {
		t.Fatal("scan ok despite a read error")
	}
	if _, ok := scanProcTaskIDs(filepath.Join(root, "missing")); ok {
		t.Fatal("scan ok for an unreadable proc root")
	}
}

// CHE-3114 review P1: a nondumpable runner (prctl dumpable=0) has a root-owned,
// unreadable environ. It is still our uid, so an unreadable environ in the
// daemon's own cgroup must fail the scan, not be skipped as foreign.
func TestScanProcTaskIDsUnreadableRunnerIsNotOK(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every environ")
	}
	root := fakeProc(t, map[string]string{"101": "MULTICA_TASK_ID=task-a\x00"})
	if err := os.Chmod(filepath.Join(root, "101", "environ"), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := scanProcTaskIDs(root); ok {
		t.Fatal("scan ok despite an unreadable same-uid process in the daemon cgroup")
	}
}

// Same-uid but outside the daemon's cgroup (sshd, ssh-agent, systemd --user):
// not a runner, so unreadable is fine. Another uid is skipped without a read.
func TestScanProcTaskIDsSkipsNonRunners(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every environ")
	}
	root := fakeProc(t, map[string]string{
		"101": "MULTICA_TASK_ID=task-a\x00",
		"102": "MULTICA_TASK_ID=hidden\x00",
		"103": "MULTICA_TASK_ID=foreign\x00",
	})
	fakeProcMeta(t, filepath.Join(root, "102"), os.Getuid(), "0::/session.scope")
	fakeProcMeta(t, filepath.Join(root, "103"), os.Getuid()+1, "0::/daemon.service")
	for _, pid := range []string{"102", "103"} {
		if err := os.Chmod(filepath.Join(root, pid, "environ"), 0); err != nil {
			t.Fatal(err)
		}
	}
	ids, ok := scanProcTaskIDs(root)
	if !ok || !reflect.DeepEqual(ids, []string{"task-a"}) {
		t.Fatalf("ids = %v ok = %v, want [task-a] ok", ids, ok)
	}
}

// The heartbeat reports the scan next to the live set.
func TestHeartbeatRequestReportsHostScan(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	req := d.heartbeatRequest("rt-1")
	// The real /proc decides ok: a CI runner with a nondumpable process in the
	// daemon's cgroup fails the scan by design (CHE-6457), so assert the pairing,
	// not that the host scan succeeds.
	if req.HostScanOK == nil || *req.HostScanOK != (req.HostTaskIDs != nil) {
		t.Fatalf("host scan = ok:%v ids:%v, want ids reported exactly when ok", req.HostScanOK, req.HostTaskIDs)
	}
	d.cleanupUnconfirmed.Store(true)
	if req := d.heartbeatRequest("rt-1"); req.HostScanOK != nil || req.HostTaskIDs != nil {
		t.Fatal("host scan reported after unconfirmed cleanup")
	}
}

// CHE-7234: with the daemon in <unit>/daemon and the run leaves in
// <unit>/runs/<attempt>, an unreadable environ in a leaf is still a runner that
// may hide its task id, so the scan is not ok. The exact-cgroup check missed it.
func TestScanProcTaskIDsUnreadableRunnerInSiblingLeafIsNotOK(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every environ")
	}
	root := fakeProc(t, map[string]string{"101": "MULTICA_TASK_ID=task-a\x00"})
	if err := os.WriteFile(filepath.Join(root, "self", "cgroup"), []byte("0::/app.slice/multica.service/daemon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeProcMeta(t, filepath.Join(root, "101"), os.Getuid(), "0::/app.slice/multica.service/runs/att-1")
	if err := os.Chmod(filepath.Join(root, "101", "environ"), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := scanProcTaskIDs(root); ok {
		t.Fatal("scan ok despite an unreadable same-uid process in a run leaf of the service")
	}
}

// A process in another unit that merely shares a name prefix is not in the
// service subtree.
func TestScanProcTaskIDsSiblingUnitWithSharedPrefixIsNotInSubtree(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every environ")
	}
	root := fakeProc(t, map[string]string{"101": "MULTICA_TASK_ID=task-a\x00"})
	if err := os.WriteFile(filepath.Join(root, "self", "cgroup"), []byte("0::/app.slice/multica.service/daemon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeProcMeta(t, filepath.Join(root, "101"), os.Getuid(), "0::/app.slice/multica.service-other/x")
	if err := os.Chmod(filepath.Join(root, "101", "environ"), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := scanProcTaskIDs(root); !ok {
		t.Fatal("a different unit must be skipped, not fail the scan")
	}
}

// A readable runner in a leaf is reported like any other.
func TestScanProcTaskIDsFindsRunnerInLeaf(t *testing.T) {
	root := fakeProc(t, map[string]string{"101": "MULTICA_TASK_ID=task-leaf\x00"})
	if err := os.WriteFile(filepath.Join(root, "self", "cgroup"), []byte("0::/u/multica.service/daemon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeProcMeta(t, filepath.Join(root, "101"), os.Getuid(), "0::/u/multica.service/runs/a")
	ids, ok := scanProcTaskIDs(root)
	if !ok || !reflect.DeepEqual(ids, []string{"task-leaf"}) {
		t.Fatalf("ids = %v ok = %v", ids, ok)
	}
}
