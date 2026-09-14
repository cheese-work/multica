package dbstartup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSettingsFromEnv(t *testing.T) {
	t.Setenv(StartupTimeoutEnv, "45s")
	t.Setenv(ConnectTimeoutEnv, "2s")

	got := SettingsFromEnv()
	if got.StartupTimeout != 45*time.Second {
		t.Fatalf("StartupTimeout = %s, want 45s", got.StartupTimeout)
	}
	if got.ConnectTimeout != 2*time.Second {
		t.Fatalf("ConnectTimeout = %s, want 2s", got.ConnectTimeout)
	}
}

func TestSettingsFromEnvAllowsFailFastButKeepsConnectionTimeoutBounded(t *testing.T) {
	t.Setenv(StartupTimeoutEnv, "0")
	t.Setenv(ConnectTimeoutEnv, "0")

	got := SettingsFromEnv()
	if got.StartupTimeout != 0 {
		t.Fatalf("StartupTimeout = %s, want 0", got.StartupTimeout)
	}
	if got.ConnectTimeout != DefaultConnectTimeout {
		t.Fatalf("ConnectTimeout = %s, want %s", got.ConnectTimeout, DefaultConnectTimeout)
	}
}

func TestSettingsFromEnvSharesEntrypointStartupBudget(t *testing.T) {
	current := time.Unix(2_000_000_000, 0)
	t.Setenv(StartupTimeoutEnv, "3m")
	t.Setenv(startupStartedAtEnv, fmt.Sprintf("%d", current.Unix()))

	settings := settingsFromEnv(func() time.Time { return current })
	if got := settings.RetryOptions().Timeout; got != 3*time.Minute {
		t.Fatalf("initial retry timeout = %s, want 3m", got)
	}

	current = current.Add(70 * time.Second)
	if got := settings.RetryOptions().Timeout; got != 110*time.Second {
		t.Fatalf("remaining retry timeout = %s, want 1m50s", got)
	}

	current = current.Add(2 * time.Minute)
	if got := settings.RetryOptions().Timeout; got != 0 {
		t.Fatalf("expired retry timeout = %s, want 0", got)
	}
}

func TestParsePoolConfigAppliesConnectTimeout(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "")
	t.Setenv("PGSERVICE", "")

	cfg, err := ParsePoolConfig("postgres://user:pass@localhost:5432/db?sslmode=disable", 7*time.Second)
	if err != nil {
		t.Fatalf("ParsePoolConfig: %v", err)
	}
	if cfg.ConnConfig.ConnectTimeout != 7*time.Second {
		t.Fatalf("ConnectTimeout = %s, want 7s", cfg.ConnConfig.ConnectTimeout)
	}
}

func TestParsePoolConfigPreservesNativeConnectTimeout(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "")
	t.Setenv("PGSERVICE", "")

	tests := []struct {
		name          string
		databaseURL   string
		multicaValue  time.Duration
		wantPGXNative time.Duration
	}{
		{
			name:          "URL longer than Multica fallback",
			databaseURL:   "postgres://user:pass@localhost:5432/db?sslmode=disable&connect_timeout=30",
			multicaValue:  5 * time.Second,
			wantPGXNative: 30 * time.Second,
		},
		{
			name:          "URL shorter than explicit Multica value",
			databaseURL:   "postgres://user:pass@localhost:5432/db?sslmode=disable&connect_timeout=1",
			multicaValue:  30 * time.Second,
			wantPGXNative: time.Second,
		},
		{
			name:          "URL explicitly disables timeout",
			databaseURL:   "postgres://user:pass@localhost:5432/db?sslmode=disable&connect_timeout=0",
			multicaValue:  5 * time.Second,
			wantPGXNative: 0,
		},
		{
			name:          "keyword value",
			databaseURL:   "host=localhost port=5432 user=user password=pass dbname=db sslmode=disable connect_timeout=12",
			multicaValue:  5 * time.Second,
			wantPGXNative: 12 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParsePoolConfig(tt.databaseURL, tt.multicaValue)
			if err != nil {
				t.Fatalf("ParsePoolConfig: %v", err)
			}
			if cfg.ConnConfig.ConnectTimeout != tt.wantPGXNative {
				t.Fatalf("ConnectTimeout = %s, want native pgx value %s", cfg.ConnConfig.ConnectTimeout, tt.wantPGXNative)
			}
		})
	}
}

func TestParsePoolConfigPreservesPGConnectTimeout(t *testing.T) {
	t.Setenv("PGSERVICE", "")

	tests := []struct {
		value string
		want  time.Duration
	}{
		{value: "17", want: 17 * time.Second},
		{value: "0", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("PGCONNECT_TIMEOUT", tt.value)
			cfg, err := ParsePoolConfig("postgres://user:pass@localhost:5432/db?sslmode=disable", 5*time.Second)
			if err != nil {
				t.Fatalf("ParsePoolConfig: %v", err)
			}
			if cfg.ConnConfig.ConnectTimeout != tt.want {
				t.Fatalf("ConnectTimeout = %s, want PGCONNECT_TIMEOUT value %s", cfg.ConnConfig.ConnectTimeout, tt.want)
			}
		})
	}
}

func TestParsePoolConfigServiceConnectTimeout(t *testing.T) {
	serviceFile := writePGServiceFile(t)
	t.Setenv("PGCONNECT_TIMEOUT", "")
	t.Setenv("PGSERVICEFILE", serviceFile)

	tests := []struct {
		name        string
		serviceEnv  string
		databaseURL string
		want        time.Duration
	}{
		{
			name:        "PGSERVICE without timeout",
			serviceEnv:  "without_timeout",
			databaseURL: "",
			want:        7 * time.Second,
		},
		{
			name:        "URL service without timeout",
			databaseURL: fmt.Sprintf("postgres:///?servicefile=%s&service=without_timeout", url.QueryEscape(serviceFile)),
			want:        7 * time.Second,
		},
		{
			name:        "PGSERVICE with timeout",
			serviceEnv:  "with_timeout",
			databaseURL: "",
			want:        13 * time.Second,
		},
		{
			name:        "URL service with timeout",
			databaseURL: fmt.Sprintf("postgres:///?servicefile=%s&service=with_timeout", url.QueryEscape(serviceFile)),
			want:        13 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PGSERVICE", tt.serviceEnv)
			cfg, err := ParsePoolConfig(tt.databaseURL, 7*time.Second)
			if err != nil {
				t.Fatalf("ParsePoolConfig: %v", err)
			}
			if cfg.ConnConfig.ConnectTimeout != tt.want {
				t.Fatalf("ConnectTimeout = %s, want %s", cfg.ConnConfig.ConnectTimeout, tt.want)
			}
		})
	}
}

func writePGServiceFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pg_service.conf")
	contents := []byte(`[without_timeout]
host=localhost
port=5432
dbname=db
user=user

[with_timeout]
host=localhost
port=5432
dbname=db
user=user
connect_timeout=13
`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	return path
}

func TestRetryRecoversWithCappedExponentialBackoff(t *testing.T) {
	var attempts int
	var delays []time.Duration
	err := Retry(context.Background(), RetryOptions{
		Timeout:        time.Minute,
		InitialBackoff: time.Second,
		MaxBackoff:     5 * time.Second,
		Jitter:         func(delay time.Duration) time.Duration { return delay },
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	}, func(context.Context) error {
		attempts++
		if attempts < 5 {
			return errors.New("database unavailable")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if attempts != 5 {
		t.Fatalf("attempts = %d, want 5", attempts)
	}
	wantDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second}
	if !reflect.DeepEqual(delays, wantDelays) {
		t.Fatalf("delays = %v, want %v", delays, wantDelays)
	}
}

func TestRetryStopsWhenParentIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var attempts int
	err := Retry(ctx, RetryOptions{
		Timeout: time.Minute,
		Jitter:  func(delay time.Duration) time.Duration { return delay },
		Sleep: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	}, func(context.Context) error {
		attempts++
		return errors.New("database unavailable")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Retry error = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestRetryStopsAtTimeout(t *testing.T) {
	var attempts int
	err := Retry(context.Background(), RetryOptions{
		Timeout: 5 * time.Millisecond,
		Jitter:  func(delay time.Duration) time.Duration { return delay },
	}, func(context.Context) error {
		attempts++
		return errors.New("database unavailable")
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Retry error = %v, want context.DeadlineExceeded", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestRetryTimeoutPreservesLastError(t *testing.T) {
	wantErr := &pgconn.PgError{Code: "57P03", Message: "database is starting"}
	err := Retry(context.Background(), RetryOptions{
		Timeout: time.Minute,
		Sleep: func(context.Context, time.Duration) error {
			return context.DeadlineExceeded
		},
	}, func(context.Context) error {
		return wantErr
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Retry error = %v, want context.DeadlineExceeded", err)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Retry error = %v, want wrapped last error %v", err, wantErr)
	}
}

func TestRetryWithZeroTimeoutAttemptsOnce(t *testing.T) {
	wantErr := errors.New("database unavailable")
	var attempts int
	err := Retry(context.Background(), RetryOptions{}, func(context.Context) error {
		attempts++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Retry error = %v, want %v", err, wantErr)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestRetryStopsImmediatelyForPermanentError(t *testing.T) {
	wantErr := &pgconn.PgError{Code: "42601", Message: "syntax error"}
	var attempts int
	err := Retry(context.Background(), RetryOptions{
		Timeout:     time.Minute,
		ShouldRetry: IsTransientDatabaseError,
	}, func(context.Context) error {
		attempts++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Retry error = %v, want %v", err, wantErr)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestRetryCanLeaveLongOperationOnParentContext(t *testing.T) {
	err := Retry(context.Background(), RetryOptions{
		Timeout:                   time.Minute,
		AllowOperationPastTimeout: true,
	}, func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); ok {
			return errors.New("operation unexpectedly inherited retry deadline")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
}

func TestIsTransientDatabaseError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "connection exception", err: &pgconn.PgError{Code: "08006"}, want: true},
		{name: "database starting", err: &pgconn.PgError{Code: "57P03"}, want: true},
		{name: "too many connections", err: &pgconn.PgError{Code: "53300"}, want: true},
		{name: "syntax error", err: &pgconn.PgError{Code: "42601"}, want: false},
		{name: "connection deadline", err: context.DeadlineExceeded, want: true},
		{name: "cancelled", err: context.Canceled, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransientDatabaseError(tt.err); got != tt.want {
				t.Fatalf("IsTransientDatabaseError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryRetriesConnectionHandshakeTimeout(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "")
	t.Setenv("PGSERVICE", "")

	address := startStalledDatabaseListener(t)
	pool, err := NewPool(
		context.Background(),
		fmt.Sprintf("postgres://user:pass@%s/db?sslmode=disable", address),
		30*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	var attempts int
	err = Retry(context.Background(), RetryOptions{
		Timeout:        time.Second,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Jitter:         func(delay time.Duration) time.Duration { return delay },
		ShouldRetry:    IsTransientDatabaseError,
	}, func(ctx context.Context) error {
		attempts++
		return pool.Ping(ctx)
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Retry error = %v, want context.DeadlineExceeded", err)
	}
	if attempts < 2 {
		t.Fatalf("attempts = %d, want at least 2 for a connection handshake timeout", attempts)
	}
}

func TestIsTransientDatabaseErrorFromRefusedConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	pool, err := NewPool(
		context.Background(),
		fmt.Sprintf("postgres://user:pass@%s/db?sslmode=disable", address),
		100*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	err = pool.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping unexpectedly succeeded")
	}
	if !IsTransientDatabaseError(err) {
		t.Fatalf("IsTransientDatabaseError(%v) = false, want true", err)
	}
}

func startStalledDatabaseListener(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var (
		mu          sync.Mutex
		connections []net.Conn
	)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections = append(connections, conn)
			mu.Unlock()
		}
	}()

	t.Cleanup(func() {
		_ = listener.Close()
		<-acceptDone
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
	})

	return listener.Addr().String()
}

func TestParseGUCDurationMs(t *testing.T) {
	cases := []struct {
		raw     string
		wantMs  int64
		wantErr bool
	}{
		{raw: "0", wantMs: 0},
		{raw: "1000ms", wantMs: 1000},
		{raw: "1s", wantMs: 1000},
		{raw: "2min", wantMs: 120000},
		{raw: "1h", wantMs: 3600000},
		{raw: "1d", wantMs: 86400000},
		{raw: "  500ms  ", wantMs: 500},
		{raw: "500", wantMs: 500},
		{raw: "not-a-duration", wantErr: true},
		{raw: "500xyz", wantErr: true},
		{raw: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseGUCDurationMs(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseGUCDurationMs(%q): expected error, got %d", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseGUCDurationMs(%q): unexpected error: %v", tc.raw, err)
			continue
		}
		if got != tc.wantMs {
			t.Errorf("parseGUCDurationMs(%q) = %d, want %d", tc.raw, got, tc.wantMs)
		}
	}
}

func TestNewPoolWithEnforcedTimeoutsRejectsNonPositiveInputs(t *testing.T) {
	cases := []EnforcedTimeouts{
		{StatementTimeout: 0, LockTimeout: time.Second},
		{StatementTimeout: time.Second, LockTimeout: 0},
		{StatementTimeout: -time.Second, LockTimeout: time.Second},
		{StatementTimeout: time.Second, LockTimeout: -time.Second},
	}
	for _, tc := range cases {
		_, err := NewPoolWithEnforcedTimeouts(context.Background(), "postgres://u:p@localhost:5432/d", 0, tc)
		if err == nil {
			t.Errorf("NewPoolWithEnforcedTimeouts(%+v): expected a validation error for a non-positive timeout, got nil", tc)
		}
	}
}

// TestEnforcedTimeoutsDefeatConnectionStringBypass proves the fix for the
// P1 finding that a connection-string "options=" parameter overrides
// PGOPTIONS/role-database defaults (pgx applies connection-string
// settings above both — see pgconn/config.go's precedence). It requires
// a real reachable Postgres via MULTICA_TEST_D2_BYPASS_DATABASE_URL and
// skips (not fails) when that is not set, since it needs a live database
// this package's other tests do not otherwise require; deploy/cd/
// test-migrate-supervised.sh sets it against a throwaway container.
func TestEnforcedTimeoutsDefeatConnectionStringBypass(t *testing.T) {
	dbURL := os.Getenv("MULTICA_TEST_D2_BYPASS_DATABASE_URL")
	if dbURL == "" {
		t.Skip("MULTICA_TEST_D2_BYPASS_DATABASE_URL not set; skipping (requires a real reachable Postgres)")
	}
	// The caller-supplied URL is expected to carry
	// options=-c%20statement_timeout%3D0%20-c%20lock_timeout%3D0, the
	// exact bypass attempt from the review: a client trying to disable
	// both timeouts via the connection string itself.
	pool, err := NewPoolWithEnforcedTimeouts(context.Background(), dbURL, 5*time.Second, EnforcedTimeouts{
		StatementTimeout: time.Second,
		LockTimeout:      time.Second,
	})
	if err != nil {
		t.Fatalf("NewPoolWithEnforcedTimeouts: %v", err)
	}
	defer pool.Close()

	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire (AfterConnect should have enforced, not rejected the connection): %v", err)
	}
	defer conn.Release()

	var statementTimeout, lockTimeout string
	if err := conn.QueryRow(context.Background(), "SHOW statement_timeout").Scan(&statementTimeout); err != nil {
		t.Fatalf("SHOW statement_timeout: %v", err)
	}
	if err := conn.QueryRow(context.Background(), "SHOW lock_timeout").Scan(&lockTimeout); err != nil {
		t.Fatalf("SHOW lock_timeout: %v", err)
	}
	if statementTimeout != "1s" {
		t.Fatalf("expected statement_timeout=1s despite the URL's options=... bypass attempt, got %q — bypass succeeded", statementTimeout)
	}
	if lockTimeout != "1s" {
		t.Fatalf("expected lock_timeout=1s despite the URL's options=... bypass attempt, got %q — bypass succeeded", lockTimeout)
	}
	t.Logf("bypass defeated: statement_timeout=%s lock_timeout=%s despite a connection-string options= bypass attempt", statementTimeout, lockTimeout)
}
