package main

import "context"

// A supervisor alone creates the successor after native shutdown admission.
type daemonRestartSupervisor interface {
	wait(context.Context, string, int, int) error
}

var findDaemonRestartSupervisor = platformDaemonRestartSupervisor
