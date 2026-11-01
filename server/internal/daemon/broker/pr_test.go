package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func (r *rig) pr(t testing.TB, body string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", r.socket)
	}}}
	resp, err := c.Post("http://broker/pr", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func TestPRCreateUsesPinnedHeadAndWritePermission(t *testing.T) {
	t.Parallel()
	r, f, _ := tokenRig(t, nil)
	code, body := r.pr(t, `{"op":"create","title":"T","body":"B","base":"main","draft":true}`)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var out struct {
		Number int
		URL    string
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Number != 1 || out.URL == "" {
		t.Fatalf("result %q (%v)", body, err)
	}
	if p := f.prs[1]; p == nil || p.HeadRef != r.branch || p.Base != "main" || p.Title != "T" || !p.Draft {
		t.Fatalf("created %+v, want head %s", p, r.branch)
	}
	want := mintReq{RepositoryIDs: []int64{42}, Permissions: map[string]string{"contents": "write", "pull_requests": "write"}}
	if len(f.mints) != 1 || !reflect.DeepEqual(f.mints[0], want) {
		t.Fatalf("mint %+v, want %+v", f.mints, want)
	}
	if !reflect.DeepEqual(f.revoked, f.minted) || f.liveCount() != 0 {
		t.Fatalf("minted %v revoked %v", f.minted, f.revoked)
	}
}

func TestPRRefusals(t *testing.T) {
	t.Parallel()
	r, f, _ := tokenRig(t, nil)
	cases := map[string]struct {
		body string
		code int
	}{
		"base not allowed":   {`{"op":"create","title":"T","base":"release"}`, 403},
		"head supplied":      {`{"op":"create","title":"T","base":"main","head":"main"}`, 400},
		"merge":              {`{"op":"merge","number":1}`, 400},
		"approve":            {`{"op":"approve","number":1}`, 400},
		"edit nothing":       {`{"op":"edit","number":1}`, 400},
		"edit base":          {`{"op":"edit","number":1,"base":"release"}`, 400},
		"create no title":    {`{"op":"create","base":"main"}`, 400},
		"not json":           {`junk`, 400},
		"comment no number":  {`{"op":"comment","body":"x"}`, 400},
		"comment empty body": {`{"op":"comment","number":1}`, 400},
	}
	f.addPR(r.branch, 42)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if code, body := r.pr(t, c.body); code != c.code {
				t.Fatalf("status %d (%s), want %d", code, body, c.code)
			}
			if w := f.writeCalls(); len(w) != 0 {
				t.Fatalf("GitHub was written: %v", w)
			}
		})
	}
	if len(f.minted) != 0 {
		t.Fatalf("minted %v for refused requests", f.minted)
	}
}

func TestPROperationsOnOwnPR(t *testing.T) {
	t.Parallel()
	r, f, _ := tokenRig(t, nil)
	own := f.addPR(r.branch, 42)
	for _, body := range []string{
		`{"op":"edit","number":1,"title":"new"}`,
		`{"op":"ready","number":1}`,
		`{"op":"comment","number":1,"body":"hello"}`,
	} {
		if code, out := r.pr(t, body); code != 200 {
			t.Fatalf("%s: status %d: %s", body, code, out)
		}
	}
	if own.Title != "new" || own.Body != "b" || !own.Ready || !reflect.DeepEqual(own.Comment, []string{"hello"}) {
		t.Fatalf("pr after operations: %+v", own)
	}
	if len(f.minted) != 3 || len(f.revoked) != 3 || f.liveCount() != 0 {
		t.Fatalf("minted %d revoked %d live %d", len(f.minted), len(f.revoked), f.liveCount())
	}
}

func TestPROperationsRefuseForeignPR(t *testing.T) {
	t.Parallel()
	r, f, _ := tokenRig(t, nil)
	other := f.addPR("agent/y/other-task", 42)
	fork := f.addPR(r.branch, 99) // same branch name, different repository
	for _, p := range []*fakePR{other, fork} {
		for _, op := range []string{
			`{"op":"edit","number":N,"title":"x"}`,
			`{"op":"ready","number":N}`,
			`{"op":"comment","number":N,"body":"x"}`,
		} {
			body := strings.Replace(op, "N", string(rune('0'+p.Number)), 1)
			if code, out := r.pr(t, body); code != 403 {
				t.Fatalf("%s: status %d (%s), want 403", body, code, out)
			}
		}
	}
	if w := f.writeCalls(); len(w) != 0 {
		t.Fatalf("GitHub was written: %v", w)
	}
	if f.liveCount() != 0 {
		t.Fatal("a token outlived a refused operation")
	}
}

func TestPRWithoutGitHubAccess(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.start(t)
	if code, _ := r.pr(t, `{"op":"comment","number":1,"body":"x"}`); code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", code)
	}
}

func TestPRClient(t *testing.T) {
	t.Parallel()
	r, f, _ := tokenRig(t, nil)
	title := "from client"
	res, err := PR(context.Background(), r.socket, PRRequest{Op: "create", Title: &title, Base: "main"})
	if err != nil || res.Number != 1 || f.prs[1].Title != title {
		t.Fatalf("create: %+v, %v", res, err)
	}
	if _, err := PR(context.Background(), r.socket, PRRequest{Op: "merge", Number: 1}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("merge: %v, want a 400 refusal", err)
	}
}
