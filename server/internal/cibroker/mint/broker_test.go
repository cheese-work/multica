package mint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

var phase1Ceiling = Permissions{
	"contents": "read", "actions": "read", "checks": "read", "statuses": "read",
	"pull_requests": "read", "issues": "read", "metadata": "read",
}

type env struct {
	b     *Broker
	min   *fakeMinter
	lim   *fakeLimiter
	audit *memAudit
	clk   *fakeClock
	logs  *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := newClock()
	lim := &fakeLimiter{}
	lim.set(reading(clk.Now(), 5000, 5000), nil)
	logs := &bytes.Buffer{}
	e := &env{min: &fakeMinter{}, lim: lim, audit: &memAudit{}, clk: clk, logs: logs}
	e.b = New(Config{
		Minter:   e.min,
		Quota:    NewQuota(lim, clk.Now),
		Throttle: NewThrottle(clk.Now),
		Audit:    e.audit,
		Ceiling:  phase1Ceiling,
		Now:      clk.Now,
		Logger:   slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	return e
}

func req(run string) Request {
	return Request{
		RepositoryID:      42,
		Repository:        "cheese-work/multica",
		RunID:             run,
		RunAttempt:        "1",
		WorkflowRef:       "cheese-work/multica/.github/workflows/cd-deploy.yml@refs/heads/main",
		Event:             "push",
		Ref:               "refs/heads/main",
		RunnerEnvironment: "self-hosted",
		Environment:       "c00-runtime",
		Wanted:            Permissions{"contents": "read", "actions": "write"},
		EntryMax:          Permissions{"contents": "read", "actions": "read", "checks": "read"},
	}
}

func TestIssueMintsIntersectionForCallerRepoAndAudits(t *testing.T) {
	e := newEnv(t)
	tok, err := e.b.Issue(context.Background(), req("1001"))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value.Reveal() == "" {
		t.Fatal("empty token")
	}
	if len(e.min.minted) != 1 || e.min.minted[0].repoID != 42 {
		t.Fatalf("minted = %+v, want one token for repo 42", e.min.minted)
	}
	want := Permissions{"contents": "read", "actions": "read"}
	if fmt.Sprint(e.min.minted[0].perms) != fmt.Sprint(want) {
		t.Fatalf("perms = %v, want %v", e.min.minted[0].perms, want)
	}
	rows := e.audit.all()
	if len(rows) != 1 || rows[0].Result != "issued" || rows[0].RunID != "1001" || rows[0].Repository != "cheese-work/multica" {
		t.Fatalf("audit = %+v", rows)
	}
	if rows[0].Environment != "c00-runtime" || rows[0].Event != "push" || rows[0].Ref != "refs/heads/main" {
		t.Fatalf("audit lost request fields: %+v", rows[0])
	}
}

func TestIssueRefusesWhenIntersectionEmpty(t *testing.T) {
	e := newEnv(t)
	r := req("1")
	r.Wanted = Permissions{"statuses": "write"}
	_, err := e.b.Issue(context.Background(), r)
	wantRefusal(t, err, http.StatusForbidden, "no_permissions")
	if len(e.min.minted) != 0 {
		t.Fatal("minted a token for an empty intersection")
	}
	rows := e.audit.all()
	if len(rows) != 1 || rows[0].Result != "refused" || rows[0].Reason != "no_permissions" {
		t.Fatalf("audit = %+v", rows)
	}
}

func TestIssueSerializesConcurrentRequestsAndHonoursQuotaAcrossThem(t *testing.T) {
	e := newEnv(t)
	e.lim.set(reading(e.clk.Now(), 700, 5000), nil) // admits 3, then 400 < 500
	e.min.hold = 5 * 1e6                            // 5 ms, widens the race window
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, refused := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.b.Issue(context.Background(), req(fmt.Sprintf("run-%d", i)))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				refused++
			}
		}(i)
	}
	wg.Wait()
	if ok != 3 || refused != 9 {
		t.Fatalf("issued %d refused %d, want 3 and 9", ok, refused)
	}
	if got := e.min.maxSeen.Load(); got != 1 {
		t.Fatalf("max concurrent mints = %d, want 1 (one mutex)", got)
	}
	if got := e.lim.calls.Load(); got != 1 {
		t.Fatalf("rate_limit calls = %d, want 1", got)
	}
	if rows := e.audit.all(); len(rows) != 12 {
		t.Fatalf("audit rows = %d, want one per request (12)", len(rows))
	}
}

func TestIssueConcurrentSameRunHonoursThrottle(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.b.Issue(context.Background(), req("same-run")); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != MaxPerRun {
		t.Fatalf("issued %d for one run attempt, want %d", ok, MaxPerRun)
	}
}

func TestIssueThrottleRefusalIs429AndAudited(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < MaxPerRun; i++ {
		if _, err := e.b.Issue(context.Background(), req("r")); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.b.Issue(context.Background(), req("r"))
	wantRefusal(t, err, http.StatusTooManyRequests, "throttle_run")
	rows := e.audit.all()
	if last := rows[len(rows)-1]; last.Result != "refused" || last.Reason != "throttle_run" {
		t.Fatalf("last audit = %+v", last)
	}
}

func TestIssueQuotaRefusalsDoNotMint(t *testing.T) {
	e := newEnv(t)
	e.lim.set(reading(e.clk.Now(), 100, 5000), nil)
	_, err := e.b.Issue(context.Background(), req("1"))
	wantRefusal(t, err, http.StatusTooManyRequests, "quota_core")
	e.lim.set(Reading{}, errBoom)
	e.clk.Advance(ReadingMaxAge + 1)
	_, err = e.b.Issue(context.Background(), req("2"))
	wantRefusal(t, err, http.StatusServiceUnavailable, "quota_unavailable")
	if len(e.min.minted) != 0 {
		t.Fatal("minted under a quota refusal")
	}
}

func TestIssueMintFailureIs503(t *testing.T) {
	e := newEnv(t)
	e.min.err = errBoom
	_, err := e.b.Issue(context.Background(), req("1"))
	wantRefusal(t, err, http.StatusServiceUnavailable, "mint_failed")
	rows := e.audit.all()
	if len(rows) != 1 || rows[0].Result != "refused" || rows[0].Reason != "mint_failed" {
		t.Fatalf("audit = %+v", rows)
	}
}

func TestIssueAuditFailureWithFailedRevokeLogsALiveToken(t *testing.T) {
	e := newEnv(t)
	e.audit.err = errBoom
	e.min.revokeErr = errBoom
	_, err := e.b.Issue(context.Background(), req("1"))
	wantRefusal(t, err, http.StatusServiceUnavailable, "audit_unavailable")
	logs := e.logs.String()
	if !strings.Contains(logs, "revoke failed") || strings.Contains(logs, "token revoked") {
		t.Fatalf("log must report the failed revoke and not claim a revoke:\n%s", logs)
	}
	if strings.Contains(logs, "ghs_SECRETTOKEN") {
		t.Fatalf("token in logs:\n%s", logs)
	}
}

func TestIssueAuditFailureRevokesTokenAndRefuses(t *testing.T) {
	e := newEnv(t)
	e.audit.err = errBoom
	_, err := e.b.Issue(context.Background(), req("1"))
	wantRefusal(t, err, http.StatusServiceUnavailable, "audit_unavailable")
	if len(e.min.minted) != 1 || len(e.min.revoked) != 1 || e.min.revoked[0] != "ghs_SECRETTOKEN0001" {
		t.Fatalf("minted %d revoked %v, want the unaudited token revoked", len(e.min.minted), e.min.revoked)
	}
}

func TestIssueContextCancelledWhileWaitingForMutex(t *testing.T) {
	e := newEnv(t)
	e.b.sem <- struct{}{} // another request holds the mutex
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.b.Issue(ctx, req("1")); err == nil {
		t.Fatal("want an error when the context ends while queued")
	}
	if len(e.min.minted) != 0 {
		t.Fatal("minted after cancel")
	}
}

func TestNoTokenInLogsAuditOrFormatting(t *testing.T) {
	e := newEnv(t)
	tok, err := e.b.Issue(context.Background(), req("1"))
	if err != nil {
		t.Fatal(err)
	}
	e.min.err = fmt.Errorf("upstream said token %s is bad", "ghs_SECRETTOKEN0001")
	_, _ = e.b.Issue(context.Background(), req("2")) // failure path
	e.min.err = nil
	e.audit.err = errBoom
	_, _ = e.b.Issue(context.Background(), req("3")) // audit-failure path

	secret := tok.Value.Reveal()
	if strings.Contains(e.logs.String(), "ghs_SECRETTOKEN") {
		t.Fatalf("token in logs:\n%s", e.logs.String())
	}
	rowsJSON, _ := json.Marshal(e.audit.all())
	if strings.Contains(string(rowsJSON), "ghs_") {
		t.Fatalf("token in audit rows: %s", rowsJSON)
	}
	for _, out := range []string{
		fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s %q", tok.Value, tok, tok, tok.Value, tok.Value),
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("Secret leaks through formatting: %s", out)
		}
	}
	buf := &bytes.Buffer{}
	slog.New(slog.NewJSONHandler(buf, nil)).Info("x", "token", tok.Value, "tok", tok)
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("Secret leaks through slog: %s", buf)
	}
	j, _ := json.Marshal(tok)
	if strings.Contains(string(j), secret) {
		t.Fatalf("Secret leaks through json: %s", j)
	}
}
