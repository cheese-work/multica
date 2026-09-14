package dbstartup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StartupTimeoutEnv = "MULTICA_DATABASE_STARTUP_TIMEOUT"
	ConnectTimeoutEnv = "MULTICA_DATABASE_CONNECT_TIMEOUT"
	// startupStartedAtEnv is set by the container entrypoint so the migrator
	// and API server consume one shared retry budget. It is intentionally not
	// a user-facing setting.
	startupStartedAtEnv = "MULTICA_INTERNAL_DATABASE_STARTUP_STARTED_AT_UNIX"

	DefaultStartupTimeout = 3 * time.Minute
	DefaultConnectTimeout = 5 * time.Second
	defaultInitialBackoff = time.Second
	defaultMaxBackoff     = 30 * time.Second
)

var keywordConnectTimeoutPattern = regexp.MustCompile(`(?:^|\s)connect_timeout\s*=`)

// Settings bounds database startup retries and individual connection attempts.
type Settings struct {
	StartupTimeout time.Duration
	ConnectTimeout time.Duration

	startupDeadline time.Time
	now             func() time.Time
}

// SettingsFromEnv reads the shared startup settings used by the migrator and
// API server. A zero startup timeout preserves an explicit fail-fast option.
func SettingsFromEnv() Settings {
	return settingsFromEnv(time.Now)
}

func settingsFromEnv(now func() time.Time) Settings {
	startupTimeout := envDuration(StartupTimeoutEnv, DefaultStartupTimeout, true)
	settings := Settings{
		StartupTimeout: startupTimeout,
		ConnectTimeout: envDuration(ConnectTimeoutEnv, DefaultConnectTimeout, false),
		now:            now,
	}
	if startupTimeout <= 0 {
		return settings
	}

	startedAt := now()
	if raw := os.Getenv(startupStartedAtEnv); raw != "" {
		unixSeconds, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && unixSeconds > 0 {
			startedAt = time.Unix(unixSeconds, 0)
		} else {
			if err == nil {
				err = errors.New("timestamp must be positive")
			}
			slog.Warn("invalid internal database startup timestamp, starting a new retry budget",
				"name", startupStartedAtEnv,
				"value", raw,
				"error", err,
			)
		}
	}
	settings.startupDeadline = startedAt.Add(startupTimeout)
	return settings
}

// ParsePoolConfig applies connectTimeout as a compatibility fallback. Native
// pgx connect_timeout settings take precedence because pgx configures both its
// whole-connection deadline and DialFunc from the same value.
func ParsePoolConfig(databaseURL string, connectTimeout time.Duration) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if pgxConnectTimeoutConfigured(databaseURL, cfg.ConnConfig.ConnectTimeout) {
		return cfg, nil
	}
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	dialer := &net.Dialer{Timeout: connectTimeout}
	cfg.ConnConfig.DialFunc = dialer.DialContext
	return cfg, nil
}

func pgxConnectTimeoutConfigured(databaseURL string, parsedTimeout time.Duration) bool {
	if parsedTimeout != 0 || os.Getenv("PGCONNECT_TIMEOUT") != "" {
		return true
	}

	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		parsedURL, err := url.Parse(databaseURL)
		if err == nil {
			query := parsedURL.Query()
			_, hasConnectTimeout := query["connect_timeout"]
			return hasConnectTimeout
		}
	}

	return keywordConnectTimeoutPattern.MatchString(databaseURL)
}

// NewPool creates a startup pool with the shared connection timeout.
func NewPool(ctx context.Context, databaseURL string, connectTimeout time.Duration) (*pgxpool.Pool, error) {
	cfg, err := ParsePoolConfig(databaseURL, connectTimeout)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// EnforcedTimeouts bounds every physical connection a pool opens, including
// connections opened later for pool "hooks" that use a *pgxpool.Pool rather
// than a single pinned connection (see cmd/migrate/main.go's
// preMigrationHook). It exists because pgx applies connection-string
// settings (including a client-supplied "options=-c statement_timeout=..."
// query parameter) before any Go-side default, so a value set only via
// PGOPTIONS or a role/database default can be silently overridden by
// whatever DATABASE_URL itself carries — pgconn/config.go's precedence
// puts the connection string's own settings above both. Enforcing here
// instead runs a SET on every connection strictly after pgx has finished
// applying the connection string, so this value is always the last one
// applied and therefore wins regardless of what the URL requested.
type EnforcedTimeouts struct {
	// StatementTimeout and LockTimeout must both be positive; a value that
	// would render as 0 in Postgres means "disabled," so callers must
	// reject that before constructing this struct rather than silently
	// proceeding with no timeout.
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	// OnNotice, when set, is attached to the resulting pool's ConnConfig
	// exactly like the plain NewPool path — this enforced-timeout pool
	// still goes through ParsePoolConfig internally, so the same
	// pgconn.NoticeHandler applies. nil preserves prior behavior (no
	// notice forwarding) for every existing caller.
	OnNotice pgconn.NoticeHandler
}

// NewPoolWithEnforcedTimeouts builds a pool exactly like NewPool, then adds
// an AfterConnect hook that sets and reads back statement_timeout/
// lock_timeout on every connection the pool opens for the lifetime of the
// pool — the pinned advisory-lock connection the migration loop acquires
// and every separate connection its hooks open on the shared pool alike.
// If either read-back value does not match what was requested (including
// rendering as 0, Postgres's "disabled" sentinel, which would mean some
// other setting source won), the connection is rejected outright rather
// than silently used with a weaker or absent timeout.
func NewPoolWithEnforcedTimeouts(ctx context.Context, databaseURL string, connectTimeout time.Duration, enforced EnforcedTimeouts) (*pgxpool.Pool, error) {
	if enforced.StatementTimeout <= 0 || enforced.LockTimeout <= 0 {
		return nil, errors.New("enforced statement/lock timeout must be positive; a value that renders as 0 in Postgres means disabled")
	}
	cfg, err := ParsePoolConfig(databaseURL, connectTimeout)
	if err != nil {
		return nil, err
	}
	if enforced.OnNotice != nil {
		cfg.ConnConfig.OnNotice = enforced.OnNotice
	}
	statementTimeoutMs := enforced.StatementTimeout.Milliseconds()
	lockTimeoutMs := enforced.LockTimeout.Milliseconds()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", statementTimeoutMs)); err != nil {
			return fmt.Errorf("enforce statement_timeout: %w", err)
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET lock_timeout = %d", lockTimeoutMs)); err != nil {
			return fmt.Errorf("enforce lock_timeout: %w", err)
		}
		var observedStatementTimeoutRaw, observedLockTimeoutRaw string
		if err := conn.QueryRow(ctx, "SHOW statement_timeout").Scan(&observedStatementTimeoutRaw); err != nil {
			return fmt.Errorf("read back statement_timeout: %w", err)
		}
		if err := conn.QueryRow(ctx, "SHOW lock_timeout").Scan(&observedLockTimeoutRaw); err != nil {
			return fmt.Errorf("read back lock_timeout: %w", err)
		}
		observedStatementTimeoutMs, err := parseGUCDurationMs(observedStatementTimeoutRaw)
		if err != nil {
			return fmt.Errorf("parse read-back statement_timeout %q: %w", observedStatementTimeoutRaw, err)
		}
		observedLockTimeoutMs, err := parseGUCDurationMs(observedLockTimeoutRaw)
		if err != nil {
			return fmt.Errorf("parse read-back lock_timeout %q: %w", observedLockTimeoutRaw, err)
		}
		if observedStatementTimeoutMs <= 0 {
			return fmt.Errorf("statement_timeout reads back as %dms (0 means disabled) after enforcing %dms — a different setting source won", observedStatementTimeoutMs, statementTimeoutMs)
		}
		if observedLockTimeoutMs <= 0 {
			return fmt.Errorf("lock_timeout reads back as %dms (0 means disabled) after enforcing %dms — a different setting source won", observedLockTimeoutMs, lockTimeoutMs)
		}
		if observedStatementTimeoutMs != statementTimeoutMs {
			return fmt.Errorf("statement_timeout reads back as %dms, expected exactly %dms", observedStatementTimeoutMs, statementTimeoutMs)
		}
		if observedLockTimeoutMs != lockTimeoutMs {
			return fmt.Errorf("lock_timeout reads back as %dms, expected exactly %dms", observedLockTimeoutMs, lockTimeoutMs)
		}
		return nil
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// parseGUCDurationMs parses a PostgreSQL GUC duration setting's text
// representation (e.g. "0", "1000ms", "1s", "2min") into milliseconds. This
// is deliberately independent from any shell/psql-based parsing elsewhere
// in this codebase (see deploy/cd/quiescence.mjs's parsePgIntervalSetting)
// since this one runs inside the Go process that set the value, over the
// pgconn wire protocol, not by shelling out.
func parseGUCDurationMs(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "0" {
		return 0, nil
	}
	match := gucDurationPattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, fmt.Errorf("unrecognized duration setting %q", raw)
	}
	value, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unrecognized duration setting %q: %w", raw, err)
	}
	unit := match[2]
	if unit == "" {
		unit = "ms"
	}
	multipliers := map[string]int64{"ms": 1, "s": 1000, "min": 60000, "h": 3600000, "d": 86400000}
	multiplier, ok := multipliers[unit]
	if !ok {
		return 0, fmt.Errorf("unrecognized duration unit in %q", raw)
	}
	return value * multiplier, nil
}

var gucDurationPattern = regexp.MustCompile(`^(\d+)(ms|s|min|h|d)?$`)

// RetryEvent describes a failed attempt before the next backoff begins.
type RetryEvent struct {
	Attempt int
	Delay   time.Duration
	Err     error
}

// RetryOptions controls a bounded exponential-backoff loop. Jitter and Sleep
// are injectable so tests can cover the full sequence without wall-clock waits.
type RetryOptions struct {
	Timeout        time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Jitter         func(time.Duration) time.Duration
	Sleep          func(context.Context, time.Duration) error
	ShouldRetry    func(error) bool
	// AllowOperationPastTimeout keeps a long-running operation on the parent
	// context while the timeout still bounds subsequent retries and backoffs.
	AllowOperationPastTimeout bool
	OnRetry                   func(RetryEvent)
}

// RetryOptions returns production defaults with the startup budget remaining
// at the time of the call. Repeated phases therefore share one wall-clock
// deadline, so successful work between retry phases also consumes the budget.
// Once it expires, later phases make one fail-fast attempt instead of reserving
// a new minimum window that could exceed the container-level retry bound.
func (s Settings) RetryOptions() RetryOptions {
	timeout := s.StartupTimeout
	if timeout > 0 && !s.startupDeadline.IsZero() {
		now := s.now
		if now == nil {
			now = time.Now
		}
		timeout = s.startupDeadline.Sub(now())
		if timeout < 0 {
			timeout = 0
		}
	}
	return RetryOptions{
		Timeout:        timeout,
		InitialBackoff: defaultInitialBackoff,
		MaxBackoff:     defaultMaxBackoff,
	}
}

// Retry runs operation immediately and then retries transient failures with
// jittered exponential backoff until it succeeds, the parent is cancelled, or
// the configured startup budget expires. Timeout zero performs one attempt.
func Retry(ctx context.Context, opts RetryOptions, operation func(context.Context) error) error {
	if operation == nil {
		return errors.New("database startup operation is nil")
	}
	if opts.Timeout <= 0 {
		return operation(ctx)
	}

	retryCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	backoff := opts.InitialBackoff
	if backoff <= 0 {
		backoff = defaultInitialBackoff
	}
	maxBackoff := opts.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	jitter := opts.Jitter
	if jitter == nil {
		jitter = jitterDelay
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := retryCtx.Err(); err != nil {
			return retryStopped(attempt-1, err, lastErr)
		}

		operationCtx := retryCtx
		if opts.AllowOperationPastTimeout {
			operationCtx = ctx
		}
		err := operation(operationCtx)
		if err == nil {
			return nil
		}
		lastErr = err
		if err := retryCtx.Err(); err != nil {
			return retryStopped(attempt, err, lastErr)
		}
		if opts.ShouldRetry != nil && !opts.ShouldRetry(err) {
			return err
		}

		delay := jitter(backoff)
		if delay < 0 {
			delay = 0
		}
		if opts.OnRetry != nil {
			opts.OnRetry(RetryEvent{Attempt: attempt, Delay: delay, Err: err})
		}
		if err := sleep(retryCtx, delay); err != nil {
			cause := retryCtx.Err()
			if cause == nil {
				cause = err
			}
			return retryStopped(attempt, cause, lastErr)
		}

		if backoff < maxBackoff {
			backoff *= 2
			if backoff <= 0 || backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func retryStopped(attempts int, cause, lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("database startup stopped before an attempt completed: %w", cause)
	}
	return fmt.Errorf("database startup failed after %d attempt(s): %w (last error: %w)", attempts, cause, lastErr)
}

func jitterDelay(delay time.Duration) time.Duration {
	// Keep each delay within 80-100% so concurrent pods do not reconnect in
	// lockstep and the configured maximum remains a hard upper bound.
	return time.Duration(float64(delay) * (0.8 + rand.Float64()*0.2))
}

// IsTransientDatabaseError limits whole-migration retries to connection and
// availability failures. SQL and migration-definition errors fail immediately
// instead of consuming the startup retry budget.
func IsTransientDatabaseError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "08") ||
			pgErr.Code == "57P01" || // admin_shutdown
			pgErr.Code == "57P02" || // crash_shutdown
			pgErr.Code == "57P03" || // cannot_connect_now
			pgErr.Code == "53300" // too_many_connections
	}

	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	return pgconn.SafeToRetry(err) || pgconn.Timeout(err)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func envDuration(name string, def time.Duration, allowZero bool) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	value, err := time.ParseDuration(raw)
	valid := err == nil && (value > 0 || allowZero && value == 0)
	if !valid {
		slog.Warn("invalid env var, using default",
			"name", name,
			"value", raw,
			"default", def.String(),
			"error", err,
		)
		return def
	}
	return value
}
