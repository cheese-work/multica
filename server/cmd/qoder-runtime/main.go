// qoder-runtime runs a server-hosted Multica to Qoder Cloud Agents bridge.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/multica-ai/multica/server/internal/qoderruntime"
)

func main() {
	os.Exit(run())
}

// run returns the exit code so deferred cleanup runs before the process exits.
func run() int {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	configPath := flag.String("config", "", "JSON configuration file; supplied fields override environment variables")
	checkOnly := flag.Bool("check", false, "Verify API access without registering a runtime or running tasks")
	flag.Parse()
	cfg, err := loadConfig(*configPath, os.Getenv)
	if err != nil {
		log.Error("read bridge configuration", "error", err)
		return 1
	}
	b, err := qoderruntime.New(cfg, log)
	if err != nil {
		log.Error("invalid bridge configuration", "error", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *checkOnly {
		if err := b.Check(ctx); err != nil {
			log.ErrorContext(ctx, "bridge check failed", "error", err)
			return 1
		}
		log.InfoContext(ctx, "bridge API access verified")
		return 0
	}
	if err := b.Run(ctx); err != nil {
		log.ErrorContext(ctx, "Qoder bridge stopped", "error", err)
		return 1
	}
	return 0
}

func loadConfig(path string, getenv func(string) string) (qoderruntime.Config, error) {
	cfg := qoderruntime.Config{
		MulticaURL: getenv("MULTICA_SERVER_URL"), MulticaToken: getenv("MULTICA_TOKEN"), WorkspaceID: getenv("MULTICA_WORKSPACE_ID"),
		QoderURL: getenv("QODER_CLOUD_BASE_URL"), QoderToken: getenv("QODER_CLOUD_TOKEN"), EnvironmentID: getenv("QODER_CLOUD_ENVIRONMENT_ID"),
		StateDir: getenv("QODER_BRIDGE_STATE_DIR"), Name: getenv("QODER_BRIDGE_NAME"),
	}
	if path == "" {
		return cfg, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("cannot open configuration file")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return cfg, fmt.Errorf("cannot read configuration or file exceeds 1 MiB")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return cfg, fmt.Errorf("configuration must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid configuration JSON or unknown field")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return cfg, fmt.Errorf("configuration must contain one JSON object")
	}
	return cfg, nil
}
