//go:build linux

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const restartTestCgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/multica.service"

func TestDaemonRestartFindsCustomSystemdSupervisor(t *testing.T) {
	ops := restartSystemdOps{
		cgroup: func(pid int) (string, error) { return restartTestCgroup, nil },
		show: func(_ context.Context, unit string) (restartServiceState, error) {
			if unit != "multica.service" {
				t.Fatalf("unit = %q", unit)
			}
			return restartServiceState{id: unit, cgroup: restartTestCgroup, active: "active", sub: "running", pid: 123}, nil
		},
	}
	supervisor, err := findSystemdRestartSupervisor(context.Background(), 123, 1000, ops)
	if err != nil {
		t.Fatal(err)
	}
	if supervisor == nil {
		t.Fatal("custom multica.service was treated as unmanaged; restart would spawn outside its supervisor")
	}
}

func TestDaemonRestartSupervisorDiscoveryRefusesAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name, cg         string
		state            restartServiceState
		readErr, showErr error
		wantErr          bool
	}{
		{name: "unmanaged session", cg: "/user.slice/user-1000.slice/session-2.scope"},
		{name: "user manager session", cg: "/user.slice/user-1000.slice/user@1000.service/app.slice/terminal.scope"},
		{name: "system service", cg: "/system.slice/multica.service", wantErr: true},
		{name: "another user", cg: strings.ReplaceAll(restartTestCgroup, "1000", "1001"), wantErr: true},
		{name: "another user session", cg: "/user.slice/user-1001.slice/user@1001.service/app.slice/terminal.scope", wantErr: true},
		{name: "service descendant", cg: restartTestCgroup + "/worker", wantErr: true},
		{name: "unreadable process", readErr: errors.New("permission denied"), wantErr: true},
		{name: "manager unavailable", cg: restartTestCgroup, showErr: errors.New("no bus"), wantErr: true},
		{name: "different main PID", cg: restartTestCgroup, state: restartServiceState{id: "multica.service", pid: 456, cgroup: restartTestCgroup, active: "active", sub: "running"}, wantErr: true},
		{name: "different cgroup", cg: restartTestCgroup, state: restartServiceState{id: "multica.service", pid: 123, cgroup: "/other", active: "active", sub: "running"}, wantErr: true},
		{name: "different unit", cg: restartTestCgroup, state: restartServiceState{id: "other.service", pid: 123, cgroup: restartTestCgroup, active: "active", sub: "running"}, wantErr: true},
		{name: "stopping", cg: restartTestCgroup, state: restartServiceState{id: "multica.service", pid: 123, cgroup: restartTestCgroup, active: "deactivating", sub: "stop"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := restartSystemdOps{
				cgroup: func(int) (string, error) { return tc.cg, tc.readErr },
				show:   func(context.Context, string) (restartServiceState, error) { return tc.state, tc.showErr },
			}
			got, err := findSystemdRestartSupervisor(context.Background(), 123, 1000, ops)
			if (err != nil) != tc.wantErr || got != nil {
				t.Fatalf("supervisor = %v, err = %v, wantErr %v", got, err, tc.wantErr)
			}
		})
	}
}

func TestDaemonRestartSupervisorWait(t *testing.T) {
	old := restartServiceState{id: "multica.service", cgroup: restartTestCgroup, pid: 123, active: "active", sub: "running"}
	ready := old
	ready.pid = 456
	inactive := restartServiceState{id: "multica.service", active: "inactive", sub: "dead"}
	activating := restartServiceState{id: "multica.service", active: "activating", sub: "auto-restart"}
	stopping := old
	stopping.active = "deactivating"
	stopping.sub = "stop"
	failed := inactive
	failed.active = "failed"
	wrongGroup := ready
	wrongGroup.cgroup = "/other"
	wrongUnit := ready
	wrongUnit.id = "other.service"
	for _, tc := range []struct {
		name              string
		states            []restartServiceState
		profile           any
		healthPID         float64
		healthStatus      string
		processGroup      string
		startErr, showErr error
		noHealth          bool
		wantErr           string
		wantStarts        int
	}{
		{name: "automatic restart preserves manager", states: []restartServiceState{old, stopping, activating, ready}, profile: "work", healthPID: 456},
		{name: "clean exit starts existing inactive service", states: []restartServiceState{inactive, ready}, profile: "work", healthPID: 456, wantStarts: 1},
		{name: "start failure", states: []restartServiceState{inactive}, startErr: errors.New("start denied"), wantErr: "start denied", wantStarts: 1},
		{name: "start timeout never repeats start", states: []restartServiceState{inactive}, wantErr: "deadline exceeded", wantStarts: 1},
		{name: "old PID timeout", states: []restartServiceState{old}, wantErr: "deadline exceeded"},
		{name: "missing health timeout", states: []restartServiceState{ready}, noHealth: true, wantErr: "deadline exceeded"},
		{name: "starting health timeout", states: []restartServiceState{ready}, profile: "work", healthPID: 456, healthStatus: "starting", wantErr: "deadline exceeded"},
		{name: "service changes during verification", states: []restartServiceState{ready, old}, profile: "work", healthPID: 456, wantErr: "changed during health verification"},
		{name: "wrong profile", states: []restartServiceState{ready}, profile: "foreign", healthPID: 456, wantErr: "profile mismatch"},
		{name: "missing profile", states: []restartServiceState{ready}, healthPID: 456, wantErr: "profile mismatch"},
		{name: "wrong health PID", states: []restartServiceState{ready}, profile: "work", healthPID: 789, wantErr: "MainPID"},
		{name: "wrong process cgroup", states: []restartServiceState{ready}, profile: "work", healthPID: 456, processGroup: "/other", wantErr: "process cgroup mismatch"},
		{name: "wrong manager cgroup", states: []restartServiceState{wrongGroup}, wantErr: "cgroup changed"},
		{name: "wrong unit", states: []restartServiceState{wrongUnit}, wantErr: "identity changed"},
		{name: "service failed", states: []restartServiceState{failed}, wantErr: "failed"},
		{name: "manager lost after shutdown", states: []restartServiceState{ready}, showErr: errors.New("no bus"), wantErr: "no bus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads, starts := 0, 0
			ops := restartSystemdOps{
				show: func(_ context.Context, unit string) (restartServiceState, error) {
					if unit != "multica.service" {
						t.Fatalf("unit = %q", unit)
					}
					state := tc.states[min(reads, len(tc.states)-1)]
					reads++
					return state, tc.showErr
				},
				start: func(_ context.Context, unit string) error {
					starts++
					if unit != "multica.service" {
						t.Fatalf("unit = %q", unit)
					}
					return tc.startErr
				},
				cgroup: func(pid int) (string, error) {
					if pid != 456 {
						t.Fatalf("pid = %d", pid)
					}
					if tc.processGroup != "" {
						return tc.processGroup, nil
					}
					return restartTestCgroup, nil
				},
				health: func(_ context.Context, port int) map[string]any {
					if port != 12345 {
						t.Fatalf("port = %d", port)
					}
					if tc.noHealth {
						return nil
					}
					status := tc.healthStatus
					if status == "" {
						status = "running"
					}
					return map[string]any{"status": status, "pid": tc.healthPID, "profile": tc.profile}
				},
			}
			s := &systemdRestartSupervisor{unit: "multica.service", cgroup: restartTestCgroup, ops: ops}
			ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
			defer cancel()
			err := s.wait(ctx, "work", 12345, 123)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %s", err, tc.wantErr)
			}
			if starts != tc.wantStarts {
				t.Fatalf("start calls = %d, want %d", starts, tc.wantStarts)
			}
		})
	}
}

func TestDaemonRestartSystemdParsers(t *testing.T) {
	for _, tc := range []struct{ data, want string }{
		{"0::" + restartTestCgroup + "\n", restartTestCgroup},
		{"2:cpu:/other\n1:name=systemd:" + restartTestCgroup + "\n", restartTestCgroup},
		{"0::/\n", "/"},
		{"1:cpu:/foo", "/"}, // cgroup v1 without systemd
		{"malformed", ""}, {"0::relative", ""}, {"0::/../foo", ""},
	} {
		got, err := parseRestartCgroup(tc.data)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Errorf("cgroup(%q) = %q, %v", tc.data, got, err)
		}
	}
	valid := "Id=multica.service\nMainPID=123\nControlGroup=" + restartTestCgroup + "\nActiveState=active\nSubState=running\n"
	state, err := parseRestartServiceState(valid)
	if err != nil || state.pid != 123 || state.cgroup != restartTestCgroup {
		t.Fatalf("state = %+v, %v", state, err)
	}
	for _, data := range []string{"", "MainPID=bad", strings.Replace(valid, "MainPID=123", "MainPID=-1", 1), valid + "Id=other.service\n", strings.Replace(valid, "ControlGroup="+restartTestCgroup+"\n", "", 1)} {
		if _, err := parseRestartServiceState(data); err == nil {
			t.Errorf("accepted invalid state %q", data)
		}
	}
}
