//go:build !linux

package main

func platformDaemonRestartSupervisor(int) (daemonRestartSupervisor, error) { return nil, nil }
