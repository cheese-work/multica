package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// fakeBrowserDaemon stands in for the local daemon: it records the request and replies once.
func fakeBrowserDaemon(t *testing.T, status int, reply string, got *map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		body["_path"], body["_auth"] = r.URL.Path, r.Header.Get("Authorization")
		*got = body
		w.WriteHeader(status)
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	t.Setenv("MULTICA_DAEMON_PORT", u.Port())
	t.Setenv("MULTICA_TOKEN", "mat_agent")
}

func runBrowser(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	for _, c := range append(browserCmd.Commands(), browserCmd) { // cobra keeps flag values between runs
		c.Flags().VisitAll(func(f *pflag.Flag) { f.Value.Set(f.DefValue) })
	}
	var out, errb bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errb)
	rootCmd.SetArgs(append([]string{"browser"}, args...))
	err = rootCmd.Execute()
	return out.String(), errb.String(), err
}

const activeLease = `{"lease_id":"l1","state":"active","class":"S0","instance_id":"i1","epoch":3,` +
	`"aud":"steel-gateway/c00","gateway_url":"https://c00.example.ts.net:35443","token":"tok.secret","token_expires_at":"2026-11-18T07:00:00Z","lease_expires_at":"2026-11-18T09:00:00Z"}`

func TestBrowserLeasePrintsGatewayAndTokenToStdoutOnly(t *testing.T) {
	var got map[string]string
	fakeBrowserDaemon(t, http.StatusCreated, activeLease, &got)
	out, errOut, err := runBrowser(t, "lease", "--profile", "p1", "--decision", "d1")
	if err != nil {
		t.Fatal(err)
	}
	if got["op"] != "lease" || got["profile_id"] != "p1" || got["decision_id"] != "d1" ||
		got["_path"] != "/browser/lease" || got["_auth"] != "Bearer mat_agent" {
		t.Errorf("daemon got %v", got)
	}
	for _, want := range []string{"https://c00.example.ts.net:35443", "tok.secret", "l1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(errOut, "tok.secret") {
		t.Errorf("token leaked to stderr: %s", errOut)
	}
}

func TestBrowserLeaseQueuedPrintsPositionAndNoToken(t *testing.T) {
	var got map[string]string
	fakeBrowserDaemon(t, http.StatusAccepted, `{"lease_id":"l2","state":"queued","class":"S0","queue_position":2}`, &got)
	out, _, err := runBrowser(t, "lease", "--profile", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "queued") || !strings.Contains(out, "2") || strings.Contains(out, "token") {
		t.Errorf("queued output wrong:\n%s", out)
	}
}

func TestBrowserRenewReleaseStatusSendTheLeaseID(t *testing.T) {
	for _, op := range []string{"renew", "release", "status"} {
		var got map[string]string
		fakeBrowserDaemon(t, http.StatusOK, `{"lease_id":"l1","state":"active","class":"S0","revoked":1}`, &got)
		if _, _, err := runBrowser(t, op, "l1"); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if got["op"] != op || got["lease_id"] != "l1" {
			t.Errorf("%s: daemon got %v", op, got)
		}
	}
}

func TestBrowserLeaseJSONAddsGatewayURL(t *testing.T) {
	var got map[string]string
	fakeBrowserDaemon(t, http.StatusCreated, activeLease, &got)
	out, _, err := runBrowser(t, "lease", "--profile", "p1", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	if parsed["gateway_url"] != "https://c00.example.ts.net:35443" || parsed["token"] != "tok.secret" {
		t.Errorf("json = %v", parsed)
	}
}

func TestBrowserErrorShowsTheServerMessageNotTheBody(t *testing.T) {
	var got map[string]string
	fakeBrowserDaemon(t, http.StatusConflict, `{"error":"a decision is required","code":"decision_required"}`, &got)
	_, _, err := runBrowser(t, "lease", "--profile", "p1")
	if err == nil || !strings.Contains(err.Error(), "decision_required") {
		t.Errorf("err = %v, want the server code", err)
	}
}

func TestBrowserNeedsATask(t *testing.T) {
	t.Setenv("MULTICA_DAEMON_PORT", "")
	if _, _, err := runBrowser(t, "status", "l1"); err == nil || !strings.Contains(err.Error(), "MULTICA_DAEMON_PORT") {
		t.Errorf("err = %v, want a MULTICA_DAEMON_PORT message", err)
	}
}

// The token audience is a label, not an endpoint: with no gateway_url from the server the CLI
// prints none rather than inventing "https://c00".
func TestBrowserLeaseNeverBuildsAURLFromTheAudience(t *testing.T) {
	var got map[string]string
	fakeBrowserDaemon(t, http.StatusCreated, `{"lease_id":"l1","state":"active","aud":"steel-gateway/c00","token":"tok.secret"}`, &got)
	stdout, _, err := runBrowser(t, "lease", "--profile", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "gateway_url") || strings.Contains(stdout, "https://c00") {
		t.Errorf("stdout invents a gateway URL:\n%s", stdout)
	}
}
