package doopread

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Low-entropy filler, long enough to pass the 32-character minimum.
var token = strings.Repeat("t", MinTokenLen)

const goodBody = `{"id":"run_1","canvasId":"c1","pageId":"p1","pageIssueId":"iss-1","ruleset":"r","scanHash":null,"complete":true,"missing":[],
"frames":[{"frameId":"f1","updatedAt":1700000000000},{"frameId":"f2","updatedAt":5}],"createdAt":1,"createdBy":"x","current":true,
"authors":[{"kind":"agent","agentId":"a1","taskId":"t1","userId":null,"provider":"anthropic","model":"m","sources":["write"]},
{"kind":"user","agentId":null,"taskId":null,"userId":"u1","provider":null,"model":null,"sources":["write","copy"]}],
"provenanceComplete":true,"futureField":{"x":1}}`

type fake struct {
	srv   *httptest.Server
	calls atomic.Int32
	path  atomic.Value
	auth  atomic.Value
}

func newFake(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *fake {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.path.Store(r.URL.Path)
		f.auth.Store(r.Header.Get("Authorization"))
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) reader() *Reader {
	return New(Config{BaseURL: f.srv.URL, Token: token, AllowLoopbackHTTP: true})
}

func body(s string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(s)) }
}

func status(code int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func TestAttestationOK(t *testing.T) {
	f := newFake(t, body(goodBody))
	v, err := f.reader().Attestation(context.Background(), "run_1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if v.CanvasID != "c1" || v.PageID != "p1" || v.PageIssueID != "iss-1" || !v.Complete || !v.Current || !v.ProvenanceComplete {
		t.Errorf("view = %+v", v)
	}
	if len(v.Frames) != 2 || v.Frames[0] != (Frame{FrameID: "f1", UpdatedAt: 1700000000000}) {
		t.Errorf("frames = %+v", v.Frames)
	}
	if len(v.Authors) != 2 || v.Authors[0].Kind != "agent" || *v.Authors[0].AgentID != "a1" || *v.Authors[0].TaskID != "t1" || v.Authors[1].UserID == nil || len(v.Authors[1].Sources) != 2 {
		t.Errorf("authors = %+v", v.Authors)
	}
	if got := f.path.Load(); got != "/api/service/design-attestations/run_1" {
		t.Errorf("path = %v", got)
	}
	if got := f.auth.Load(); got != "Bearer "+token {
		t.Errorf("auth = %v", got)
	}
}

func TestAttestationPageIssueNull(t *testing.T) {
	f := newFake(t, body(strings.Replace(goodBody, `"pageIssueId":"iss-1"`, `"pageIssueId":null`, 1)))
	v, err := f.reader().Attestation(context.Background(), "run_1")
	if err != nil || v.PageIssueID != "" {
		t.Errorf("v = %+v, err = %v", v, err)
	}
}

func TestAttestationErrors(t *testing.T) {
	cases := map[string]struct {
		handler func(http.ResponseWriter, *http.Request)
		want    error
	}{
		"404":      {status(404), ErrNotFound},
		"401":      {status(401), ErrUnavailable},
		"403":      {status(403), ErrUnavailable},
		"500":      {status(500), ErrUnavailable},
		"503":      {status(503), ErrUnavailable},
		"redirect": {func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) }, ErrUnavailable},
		"oversize": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"canvasId":"` + strings.Repeat("a", 256*1024) + `"}`))
		}, ErrUnavailable},
		"not json":          {body("<html>"), ErrUnavailable},
		"empty":             {body(""), ErrUnavailable},
		"trailing data":     {body(goodBody + " {}"), ErrUnavailable},
		"missing canvasId":  {body(strings.Replace(goodBody, `"canvasId":"c1",`, "", 1)), ErrUnavailable},
		"empty pageId":      {body(strings.Replace(goodBody, `"pageId":"p1"`, `"pageId":""`, 1)), ErrUnavailable},
		"missing issue key": {body(strings.Replace(goodBody, `"pageIssueId":"iss-1",`, "", 1)), ErrUnavailable},
		"complete string":   {body(strings.Replace(goodBody, `"complete":true`, `"complete":"yes"`, 1)), ErrUnavailable},
		"missing current":   {body(strings.Replace(goodBody, `"current":true,`, "", 1)), ErrUnavailable},
		"frames not list":   {body(strings.Replace(goodBody, `"frames":[`, `"frames":{"a":[`, 1)), ErrUnavailable},
		"frame no id":       {body(strings.Replace(goodBody, `"frameId":"f1",`, "", 1)), ErrUnavailable},
		"missing authors":   {body(strings.Replace(goodBody, `"authors":`, `"authorz":`, 1)), ErrUnavailable},
		"authors null":      {body(strings.Replace(goodBody, `"authors":[`, `"authors":null,"x":[`, 1)), ErrUnavailable},
		"missing prov":      {body(strings.Replace(goodBody, `"provenanceComplete":true,`, "", 1)), ErrUnavailable},
		"id mismatch":       {body(strings.Replace(goodBody, `"id":"run_1"`, `"id":"other"`, 1)), ErrUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, tc.handler)
			_, err := f.reader().Attestation(context.Background(), "run_1")
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAttestationRedirectNotFollowed(t *testing.T) {
	target := newFake(t, body(goodBody))
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+"/x", http.StatusFound)
	})
	if _, err := f.reader().Attestation(context.Background(), "run_1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
	if target.calls.Load() != 0 {
		t.Error("the redirect target was called")
	}
}

func TestAttestationTimeout(t *testing.T) {
	release := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	start := time.Now()
	_, err := f.reader().Attestation(context.Background(), "run_1")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d < 2*time.Second || d > 5*time.Second {
		t.Errorf("returned after %v, want about 3s", d)
	}
}

func TestAttestationContextCancelled(t *testing.T) {
	f := newFake(t, body(goodBody))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.reader().Attestation(ctx, "run_1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}

func TestAttestationBadIDMakesNoCall(t *testing.T) {
	f := newFake(t, body(goodBody))
	r := f.reader()
	for _, id := range []string{"", "a/b", "a b", "..", "a?x=1", "a%2Fb", "a.b", strings.Repeat("a", 65), "é", "a\n"} {
		if _, err := r.Attestation(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("id %q: err = %v, want ErrNotFound", id, err)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("calls = %d, want 0", n)
	}
	if _, err := r.Attestation(context.Background(), strings.Repeat("a", 64)); err != nil && !errors.Is(err, ErrUnavailable) {
		t.Errorf("64-char id refused: %v", err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1 for the 64-char id", n)
	}
}

func TestAttestationNotConfigured(t *testing.T) {
	f := newFake(t, body(goodBody))
	cases := map[string]*Reader{
		"no url":                    New(Config{Token: token}),
		"no token":                  New(Config{BaseURL: f.srv.URL, AllowLoopbackHTTP: true}),
		"short token":               New(Config{BaseURL: f.srv.URL, Token: "short", AllowLoopbackHTTP: true}),
		"zero":                      New(Config{}),
		"nil reader":                nil,
		"http not loopback allowed": New(Config{BaseURL: f.srv.URL, Token: token}),
		"http remote":               New(Config{BaseURL: "http://doop.example.com", Token: token, AllowLoopbackHTTP: true}),
		"not a url":                 New(Config{BaseURL: "doop", Token: token}),
		"url with path":             New(Config{BaseURL: "https://doop.example.com/x", Token: token}),
		"userinfo":                  New(Config{BaseURL: "https://u:p@doop.example.com", Token: token}),
	}
	for name, r := range cases {
		if _, err := r.Attestation(context.Background(), "run_1"); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s: err = %v, want ErrNotConfigured", name, err)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("calls = %d, want 0", n)
	}
}

func TestFromEnv(t *testing.T) {
	f := newFake(t, body(goodBody))
	t.Setenv("MULTICA_DOOP_API_URL", "")
	t.Setenv("MULTICA_DOOP_ATTESTATION_TOKEN", "")
	if _, err := FromEnv().Attestation(context.Background(), "run_1"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("unset env: err = %v, want ErrNotConfigured", err)
	}
	// A plain-http loopback URL from the environment is refused: loopback is for tests only.
	t.Setenv("MULTICA_DOOP_API_URL", f.srv.URL)
	t.Setenv("MULTICA_DOOP_ATTESTATION_TOKEN", token)
	if _, err := FromEnv().Attestation(context.Background(), "run_1"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("http env url: err = %v, want ErrNotConfigured", err)
	}
	if f.calls.Load() != 0 {
		t.Error("no call may be made")
	}
}
