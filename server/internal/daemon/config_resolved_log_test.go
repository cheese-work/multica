package daemon

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLogResolvedConfig_NearLimitWarning(t *testing.T) {
	cases := []struct {
		name    string
		warning time.Duration
		want    string
	}{
		{"override", time.Minute, "near_limit_warning=1m0s"},
		{"disabled", 0, "near_limit_warning=0s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			d := &Daemon{
				logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
				cfg:    Config{AgentNearLimitWarning: c.warning},
			}
			d.logResolvedConfig()
			out := buf.String()
			if !strings.Contains(out, "daemon config resolved") || !strings.Contains(out, "level=DEBUG") {
				t.Fatalf("expected debug 'daemon config resolved' line, got: %s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("missing %q in: %s", c.want, out)
			}
		})
	}
}
