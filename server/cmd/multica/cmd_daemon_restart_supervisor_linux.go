//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

type restartServiceState struct {
	id, cgroup, active, sub string
	pid                     int
}

type restartSystemdOps struct {
	cgroup func(int) (string, error)
	show   func(context.Context, string) (restartServiceState, error)
	start  func(context.Context, string) error
	health func(context.Context, int) map[string]any
}

func platformDaemonRestartSupervisor(pid int) (daemonRestartSupervisor, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return findSystemdRestartSupervisor(ctx, pid, os.Getuid(), restartSystemdOps{
		cgroup: readRestartCgroup,
		show: func(ctx context.Context, unit string) (restartServiceState, error) {
			out, err := restartSystemctl(ctx, "show", "--property=Id,MainPID,ControlGroup,ActiveState,SubState", "--", unit)
			if err != nil {
				return restartServiceState{}, err
			}
			return parseRestartServiceState(string(out))
		},
		start: func(ctx context.Context, unit string) error {
			_, err := restartSystemctl(ctx, "start", "--no-block", "--", unit)
			return err
		},
		health: checkDaemonHealthOnPort,
	})
}

func restartSystemctl(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return nil, systemdCommandError(args[0], err, out)
	}
	return out, nil
}

func readRestartCgroup(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	return parseRestartCgroup(string(data))
}

func parseRestartCgroup(data string) (string, error) {
	var legacy string
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			return "", fmt.Errorf("invalid process cgroup record")
		}
		cg := fields[2]
		if !strings.HasPrefix(cg, "/") || path.Clean(cg) != cg {
			return "", fmt.Errorf("invalid process cgroup path")
		}
		if fields[0] == "0" && fields[1] == "" {
			return cg, nil
		}
		if fields[1] == "name=systemd" {
			legacy = cg
		}
	}
	if legacy != "" {
		return legacy, nil
	}
	// A valid cgroup-v1 record without a systemd hierarchy is unmanaged.
	return "/", nil
}

func parseRestartServiceState(data string) (restartServiceState, error) {
	fields := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return restartServiceState{}, fmt.Errorf("invalid systemd service state")
		}
		if _, exists := fields[key]; exists {
			return restartServiceState{}, fmt.Errorf("duplicate systemd service property %s", key)
		}
		fields[key] = value
	}
	pid, err := strconv.Atoi(fields["MainPID"])
	if err != nil || pid < 0 || fields["Id"] == "" || fields["ActiveState"] == "" || fields["SubState"] == "" {
		return restartServiceState{}, fmt.Errorf("incomplete systemd service state")
	}
	if _, ok := fields["ControlGroup"]; !ok {
		return restartServiceState{}, fmt.Errorf("missing systemd ControlGroup")
	}
	return restartServiceState{id: fields["Id"], cgroup: fields["ControlGroup"], active: fields["ActiveState"], sub: fields["SubState"], pid: pid}, nil
}

type systemdRestartSupervisor struct {
	unit, cgroup string
	ops          restartSystemdOps
}

// Discover the target's supervisor, never the invoking shell's service. A
// service descendant or a system service is ambiguous and must not select the
// portable spawn path. The user manager must confirm exact ownership.
func findSystemdRestartSupervisor(ctx context.Context, pid, uid int, ops restartSystemdOps) (daemonRestartSupervisor, error) {
	cg, err := ops.cgroup(pid)
	if err != nil {
		return nil, fmt.Errorf("identify daemon supervisor before shutdown: %w", err)
	}
	unit := path.Base(cg)
	if !strings.HasSuffix(unit, ".service") {
		for _, part := range strings.Split(cg, "/") {
			if strings.HasSuffix(part, ".service") && part != fmt.Sprintf("user@%d.service", uid) {
				return nil, fmt.Errorf("daemon is a service descendant; cannot prove supervisor ownership")
			}
		}
		return nil, nil
	}
	if !strings.Contains(cg, fmt.Sprintf("/user@%d.service/", uid)) {
		return nil, fmt.Errorf("daemon is not in this user's service manager; refusing restart")
	}
	state, err := ops.show(ctx, unit)
	if err != nil {
		return nil, fmt.Errorf("verify daemon supervisor before shutdown: %w", err)
	}
	if state.id != unit || state.pid != pid || state.cgroup != cg || state.active != "active" || state.sub != "running" {
		return nil, fmt.Errorf("daemon supervisor ownership mismatch before shutdown: %s", unit)
	}
	return &systemdRestartSupervisor{unit: unit, cgroup: cg, ops: ops}, nil
}

// Native /shutdown performs admission and cleanup. Only systemd creates the
// replacement. Never restart/stop the unit while cleanup may still be running.
func (s *systemdRestartSupervisor) wait(ctx context.Context, profile string, port, oldPID int) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	started := false
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for supervised daemon restart (%s): %w; check 'multica daemon status'", s.unit, err)
		}
		state, err := s.ops.show(ctx, s.unit)
		if err != nil {
			return fmt.Errorf("read daemon supervisor after shutdown: %w", err)
		}
		if state.id != s.unit {
			return fmt.Errorf("daemon supervisor identity changed after shutdown")
		}
		switch state.active {
		case "inactive":
			if state.pid != 0 || (state.cgroup != "" && state.cgroup != s.cgroup) {
				return fmt.Errorf("inactive daemon supervisor ownership mismatch")
			}
			if !started {
				if err := s.ops.start(ctx, s.unit); err != nil {
					return fmt.Errorf("start daemon through existing supervisor: %w", err)
				}
				started = true
			}
		case "active":
			if state.cgroup != s.cgroup {
				return fmt.Errorf("daemon supervisor cgroup changed after shutdown")
			}
			if state.pid > 0 && state.pid != oldPID && state.sub == "running" {
				healthCtx, cancel := context.WithTimeout(ctx, time.Second)
				health := s.ops.health(healthCtx, port)
				cancel()
				if daemonAlive(health) {
					gotProfile, ok := health["profile"].(string)
					if !ok || gotProfile != profile {
						return fmt.Errorf("supervised replacement profile mismatch")
					}
					pid, _ := health["pid"].(float64)
					if pid != float64(state.pid) {
						return fmt.Errorf("supervised replacement health PID does not match service MainPID")
					}
					cg, err := s.ops.cgroup(state.pid)
					if err != nil {
						return fmt.Errorf("read supervised replacement process cgroup: %w", err)
					}
					if cg != s.cgroup {
						return fmt.Errorf("supervised replacement process cgroup mismatch")
					}
					if health["status"] == "running" && ctx.Err() == nil {
						confirmed, err := s.ops.show(ctx, s.unit)
						if err != nil {
							return fmt.Errorf("confirm replacement service: %w", err)
						}
						if confirmed != state {
							return fmt.Errorf("replacement service changed during health verification")
						}
						if ctx.Err() == nil {
							return nil
						}
					}
				}
			}
		case "activating", "deactivating", "reloading":
			// Includes RestartSec's auto-restart delay. Leave the manager alone.
		case "failed":
			return fmt.Errorf("daemon supervisor %s failed; check its journal and 'multica daemon status'", s.unit)
		default:
			return fmt.Errorf("unexpected daemon supervisor state %q", state.active)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for supervised daemon restart (%s): %w; check 'multica daemon status'", s.unit, ctx.Err())
		case <-ticker.C:
		}
	}
}
