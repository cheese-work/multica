//go:build linux

package daemon

import (
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
	}
	return root
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
	if _, ok := scanProcTaskIDs(root); ok {
		t.Fatal("scan ok despite a read error")
	}
	if _, ok := scanProcTaskIDs(filepath.Join(root, "missing")); ok {
		t.Fatal("scan ok for an unreadable proc root")
	}
}

// The heartbeat reports the scan next to the live set.
func TestHeartbeatRequestReportsHostScan(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	req := d.heartbeatRequest("rt-1")
	if req.HostScanOK == nil || !*req.HostScanOK || req.HostTaskIDs == nil {
		t.Fatalf("host scan = ok:%v ids:%v, want ok with ids", req.HostScanOK, req.HostTaskIDs)
	}
	d.cleanupUnconfirmed.Store(true)
	if req := d.heartbeatRequest("rt-1"); req.HostScanOK != nil || req.HostTaskIDs != nil {
		t.Fatal("host scan reported after unconfirmed cleanup")
	}
}
