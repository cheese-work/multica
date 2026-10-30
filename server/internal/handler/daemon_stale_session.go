package handler

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	staleSessionMinutesEnv     = "MULTICA_STALE_SESSION_MINUTES"
	staleSessionMinutesDefault = 55
)

// staleSessionMinutes is how long a Claude issue session may sit idle before it
// stops being resumed. The prompt cache (Sub2API 1 h TTL and sticky binding)
// expires at 1 h, so a later resume re-writes the whole transcript at the
// cache-write price; 55 min leaves headroom for claim latency. An unset or
// invalid value uses the default.
func staleSessionMinutes() time.Duration {
	if n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(staleSessionMinutesEnv)), 10, 64); err == nil && n > 0 {
		return time.Duration(n) * time.Minute
	}
	return staleSessionMinutesDefault * time.Minute
}

// staleClaudeSession reports whether a prior Claude run ended too long ago for
// its prompt cache to still be warm. Other providers are not measured, so they
// never gate. endedAt is completed_at, falling back to started_at.
func staleClaudeSession(provider string, endedAt, startedAt pgtype.Timestamptz, now time.Time) bool {
	if provider != "claude" {
		return false
	}
	ended := endedAt
	if !ended.Valid {
		ended = startedAt
	}
	return ended.Valid && now.Sub(ended.Time) >= staleSessionMinutes()
}
