package ghstack

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// fakeGH is a stateful fake of the GitHub REST surface the executor uses,
// behind a real ghsnapshot.Client (httptest + apiBase). No live calls.
type fakeGH struct {
	mu          sync.Mutex
	def         string
	prs         map[int]fakePR
	stacks      map[int][]int // stack number -> PR numbers, bottom first
	nextStack   int
	writes      []string // "POST path body"
	conflicts   int      // next N writes answer 422
	onConflict  func(f *fakeGH)
	stacksCode  int // non-zero: every stacks read answers this status
	stacksBody  string
	dropOnWrite bool // writes succeed but the stack is not stored (read-back fails)
}

type fakePR struct {
	head, base, state string
	merged, fork      bool
}

func newFakeGH() *fakeGH {
	return &fakeGH{def: "main", prs: map[int]fakePR{}, stacks: map[int][]int{}, nextStack: 1}
}

func (f *fakeGH) pr(n int, head, base string) {
	f.prs[n] = fakePR{head: head, base: base, state: "open"}
}

func (f *fakeGH) stackOf(n int) (int, bool) {
	for id, prs := range f.stacks {
		for _, m := range prs {
			if m == n {
				return id, true
			}
		}
	}
	return 0, false
}

func (f *fakeGH) writeCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.writes) }

func prJSON(n int, p fakePR) string {
	repo := `"o/r"`
	if p.fork {
		repo = `"x/r"`
	}
	return fmt.Sprintf(`{"number":%d,"state":%q,"merged":%t,"head":{"ref":%q,"repo":{"full_name":%s}},"base":{"ref":%q}}`, n, p.state, p.merged, p.head, repo, p.base)
}

func (f *fakeGH) stackJSON(id int) string {
	var members []string
	for _, n := range f.stacks[id] {
		p := f.prs[n]
		members = append(members, fmt.Sprintf(`{"number":%d,"state":%q,"draft":false,"merged_at":null,"head":{"ref":%q,"sha":"s%d"}}`, n, p.state, p.head, n))
	}
	return fmt.Sprintf(`{"id":%d,"node_id":"n","url":"u","created_at":"t","number":%d,"base":{"ref":"main"},"open":true,"pull_requests":[%s]}`, id, id, strings.Join(members, ","))
}

func (f *fakeGH) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/repos/o/r")
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && path == "":
		fmt.Fprintf(w, `{"default_branch":%q}`, f.def)
	case r.Method == "GET" && strings.HasPrefix(path, "/pulls/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(path, "/pulls/"))
		p, ok := f.prs[n]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(prJSON(n, p)))
	case r.Method == "GET" && path == "/pulls":
		head := strings.TrimPrefix(r.URL.Query().Get("head"), "o:")
		var nums []int
		for n, p := range f.prs {
			if p.head == head && !p.fork {
				nums = append(nums, n)
			}
		}
		sort.Ints(nums)
		var out []string
		for _, n := range nums {
			out = append(out, prJSON(n, f.prs[n]))
		}
		fmt.Fprintf(w, `[%s]`, strings.Join(out, ","))
	case r.Method == "GET" && path == "/stacks":
		if f.stacksCode != 0 {
			w.WriteHeader(f.stacksCode)
			_, _ = w.Write([]byte(f.stacksBody))
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("pull_request"))
		var out []string
		if id, ok := f.stackOf(n); ok {
			out = append(out, f.stackJSON(id))
		}
		fmt.Fprintf(w, `[%s]`, strings.Join(out, ","))
	case r.Method == "POST" && strings.HasPrefix(path, "/stacks"):
		f.writes = append(f.writes, "POST "+path+" "+string(body))
		if f.conflicts > 0 {
			f.conflicts--
			if f.onConflict != nil {
				f.onConflict(f)
			}
			w.WriteHeader(422)
			return
		}
		var in struct {
			PullRequests []int `json:"pull_requests"`
		}
		_ = json.Unmarshal(body, &in)
		id := 0
		if path == "/stacks" {
			for _, n := range in.PullRequests {
				if _, ok := f.stackOf(n); ok {
					w.WriteHeader(422)
					return
				}
			}
			id = f.nextStack
			f.nextStack++
			if !f.dropOnWrite {
				f.stacks[id] = in.PullRequests
			}
		} else {
			id, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/stacks/"), "/add"))
			if !f.dropOnWrite {
				f.stacks[id] = append(f.stacks[id], in.PullRequests...)
			}
		}
		w.WriteHeader(201)
		if f.dropOnWrite {
			f.stacks[99] = in.PullRequests
			defer delete(f.stacks, 99)
			_, _ = w.Write([]byte(f.stackJSON(99)))
			return
		}
		_, _ = w.Write([]byte(f.stackJSON(id)))
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGH) client(t *testing.T) *ghsnapshot.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		f.handle(w, r)
	}))
	t.Cleanup(srv.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return ghsnapshot.NewClientWithAPIBase("1", key, srv.URL)
}

// seqModes answers Resolve from a script; the last entry repeats.
type seqModes struct {
	mu    sync.Mutex
	seq   []Resolution
	calls int
}

func (m *seqModes) Resolve(context.Context, int64, string, string) (Resolution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.calls
	if i >= len(m.seq) {
		i = len(m.seq) - 1
	}
	m.calls++
	return m.seq[i], nil
}

func res(final Mode, ws ...WorkspaceMode) Resolution {
	if len(ws) == 0 {
		ws = []WorkspaceMode{{"w1", final}}
	}
	return Resolution{Final: final, Workspaces: ws}
}

type recReporter struct {
	mu   sync.Mutex
	outs []Outcome
}

func (r *recReporter) Report(_ context.Context, o Outcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outs = append(r.outs, o)
	return nil
}

func (r *recReporter) codes() []Code {
	r.mu.Lock()
	defer r.mu.Unlock()
	var c []Code
	for _, o := range r.outs {
		c = append(c, o.Code)
	}
	return c
}

func setup(t *testing.T, f *fakeGH, modes ...Resolution) (*Executor, *recReporter, *seqModes) {
	t.Helper()
	sm := &seqModes{seq: modes}
	rep := &recReporter{}
	return &Executor{API: f.client(t), Modes: sm, Reporter: rep}, rep, sm
}

var job = func(n int) Job { return Job{InstallationID: 1, Owner: "o", Repo: "r", PR: n} }

// chain2: P1 (a -> main) and N=2 (b -> a).
func chain2() *fakeGH {
	f := newFakeGH()
	f.pr(1, "a", "main")
	f.pr(2, "b", "a")
	return f
}

func TestOffMakesNoCallsAndNoReport(t *testing.T) {
	f := chain2()
	var reads int32
	e, rep, _ := setup(t, f, res(ModeOff))
	e.API = countingAPI{e.API, &reads}
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if reads != 0 || f.writeCount() != 0 || len(rep.codes()) != 0 {
		t.Fatalf("off must do nothing: reads=%d writes=%d reports=%v", reads, f.writeCount(), rep.codes())
	}
}

type countingAPI struct {
	API
	n *int32
}

func (c countingAPI) DefaultBranch(ctx context.Context, i int64, o, r string) (string, error) {
	atomic.AddInt32(c.n, 1)
	return c.API.DefaultBranch(ctx, i, o, r)
}

func TestReportModeWritesNothingAndRecordsWouldCreate(t *testing.T) {
	f := chain2()
	e, rep, _ := setup(t, f, res(ModeReport))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if f.writeCount() != 0 {
		t.Fatalf("report mode wrote: %v", f.writes)
	}
	if !reflect.DeepEqual(rep.codes(), []Code{OutcomeWouldCreate}) {
		t.Fatalf("reports = %v", rep.codes())
	}
	if rep.outs[0].BaseRef != "a" || rep.outs[0].Job.PR != 2 {
		t.Fatalf("outcome = %+v", rep.outs[0])
	}
}

func TestLinkCreatesStackBottomFirstAndReadsBack(t *testing.T) {
	f := chain2()
	e, rep, _ := setup(t, f, res(ModeLink))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.writes, []string{`POST /stacks {"pull_requests":[1,2]}`}) {
		t.Fatalf("writes = %v", f.writes)
	}
	if !reflect.DeepEqual(rep.codes(), []Code{OutcomeCreated}) {
		t.Fatalf("reports = %v", rep.codes())
	}
}

func TestLinkExtendsExistingStackWithUnstackedAncestors(t *testing.T) {
	f := newFakeGH()
	f.pr(1, "a", "main")
	f.pr(2, "b", "a")
	f.pr(3, "c", "b")
	f.pr(4, "d", "c")
	f.stacks[7] = []int{1, 2}
	e, rep, _ := setup(t, f, res(ModeLink))
	if err := e.Run(context.Background(), job(4)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.writes, []string{`POST /stacks/7/add {"pull_requests":[3,4]}`}) {
		t.Fatalf("writes = %v", f.writes)
	}
	if !reflect.DeepEqual(rep.codes(), []Code{OutcomeExtended}) {
		t.Fatalf("reports = %v", rep.codes())
	}
}

func TestNoOpWhenAlreadyStacked(t *testing.T) {
	f := chain2()
	f.stacks[1] = []int{1, 2}
	e, rep, _ := setup(t, f, res(ModeLink))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if f.writeCount() != 0 || len(rep.codes()) != 0 {
		t.Fatalf("no-op wrote or reported: %v %v", f.writes, rep.codes())
	}
}

func TestModeLoweredWhileQueuedGivesNoWrite(t *testing.T) {
	f := chain2()
	// Job start sees link; the re-read right before the write sees report.
	e, rep, sm := setup(t, f, res(ModeLink), res(ModeReport))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if f.writeCount() != 0 {
		t.Fatalf("wrote after mode dropped: %v", f.writes)
	}
	if sm.calls < 2 {
		t.Fatalf("mode must be resolved again before the write, calls=%d", sm.calls)
	}
	if !reflect.DeepEqual(rep.codes(), []Code{OutcomeWouldCreate}) {
		t.Fatalf("reports = %v", rep.codes())
	}
}

func TestModeLoweredToOffBeforeWriteReportsNothing(t *testing.T) {
	f := chain2()
	e, rep, _ := setup(t, f, res(ModeLink), res(ModeOff))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if f.writeCount() != 0 || len(rep.codes()) != 0 {
		t.Fatalf("off before write: writes=%v reports=%v", f.writes, rep.codes())
	}
}

func Test422ReReadsAndConverges(t *testing.T) {
	f := chain2()
	f.conflicts = 1
	// Another writer stacks the pair between our write and our re-read.
	f.onConflict = func(f *fakeGH) { f.stacks[5] = []int{1, 2} }
	e, rep, _ := setup(t, f, res(ModeLink))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if f.writeCount() != 1 {
		t.Fatalf("converged state must not be written again: %v", f.writes)
	}
	if len(rep.codes()) != 0 {
		t.Fatalf("reports = %v", rep.codes())
	}
}

func Test422OnceThenWriteSucceeds(t *testing.T) {
	f := chain2()
	f.conflicts = 1
	e, rep, _ := setup(t, f, res(ModeLink))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if f.writeCount() != 2 || !reflect.DeepEqual(rep.codes(), []Code{OutcomeCreated}) {
		t.Fatalf("writes=%v reports=%v", f.writes, rep.codes())
	}
}

func Test422TwiceReportsConflict(t *testing.T) {
	f := chain2()
	f.conflicts = 5
	e, rep, _ := setup(t, f, res(ModeLink))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if f.writeCount() != 2 {
		t.Fatalf("exactly one retry expected, writes=%v", f.writes)
	}
	if !reflect.DeepEqual(rep.codes(), []Code{OutcomeConflict}) {
		t.Fatalf("reports = %v", rep.codes())
	}
}

func TestDBeforeCConvergesToOneStackAcrossTwoJobs(t *testing.T) {
	f := newFakeGH()
	f.pr(1, "a", "main")
	f.pr(2, "b", "a")
	f.pr(3, "c", "b")
	f.pr(4, "d", "c")
	f.stacks[7] = []int{1, 2}
	e, rep, _ := setup(t, f, res(ModeLink))
	var wg sync.WaitGroup
	for _, n := range []int{4, 3} { // D's event first, then C's, concurrently
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.Run(context.Background(), job(n)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.writeCount() != 1 {
		t.Fatalf("two jobs must make one write, got %v", f.writes)
	}
	if got := f.stacks[7]; !reflect.DeepEqual(got, []int{1, 2, 3, 4}) || len(f.stacks) != 1 {
		t.Fatalf("stacks = %v", f.stacks)
	}
	if !reflect.DeepEqual(rep.codes(), []Code{OutcomeExtended}) {
		t.Fatalf("reports = %v", rep.codes())
	}
}

// gateAPI blocks the first DefaultBranch call of PR gateOwner until released
// and records which PR numbers started.
type gateAPI struct {
	API
	mu      sync.Mutex
	started map[string]bool
	entered chan string
	release chan struct{}
	gateKey string
}

func (g *gateAPI) DefaultBranch(ctx context.Context, i int64, o, r string) (string, error) {
	key := o + "/" + r
	g.mu.Lock()
	first := !g.started[key]
	g.started[key] = true
	g.mu.Unlock()
	g.entered <- key
	if first && key == g.gateKey {
		<-g.release
	}
	return g.API.DefaultBranch(ctx, i, o, r)
}

func TestSameRepoJobsNeverOverlapAndOtherReposRun(t *testing.T) {
	f := chain2()
	e, _, _ := setup(t, f, res(ModeOff))
	g := &gateAPI{API: e.API, started: map[string]bool{}, entered: make(chan string, 16), release: make(chan struct{}), gateKey: "o/r"}
	e.API = g
	e.Modes = &seqModes{seq: []Resolution{res(ModeLink)}}
	go func() { _ = e.Run(context.Background(), job(2)) }()
	select {
	case k := <-g.entered:
		if k != "o/r" {
			t.Fatal("first job must enter")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first job never started")
	}
	second := make(chan struct{})
	go func() { _ = e.Run(context.Background(), job(2)); close(second) }()
	other := make(chan struct{})
	go func() {
		_ = e.Run(context.Background(), Job{InstallationID: 1, Owner: "o2", Repo: "r2", PR: 2})
		close(other)
	}()
	select {
	case k := <-g.entered:
		if k == "o/r" {
			t.Fatal("second job of the same repo started while the first holds the repo")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a job of another repo must not wait for this repo")
	}
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("other repo job never finished")
	}
	select {
	case k := <-g.entered:
		t.Fatalf("unexpected start %q", k)
	case <-time.After(150 * time.Millisecond):
	}
	close(g.release)
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("second job never ran after release")
	}
}

func TestReadFailuresBecomeReportsNotErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   Code
	}{
		{"404", 404, `{}`, OutcomeUnsupported},
		{"403", 403, `{}`, OutcomeNoPermission},
		{"shape", 200, `{"not":"a list"}`, OutcomeSchema},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := chain2()
			f.stacksCode, f.stacksBody = c.status, c.body
			e, rep, _ := setup(t, f, res(ModeLink))
			if err := e.Run(context.Background(), job(2)); err != nil {
				t.Fatalf("must not fail the caller: %v", err)
			}
			if f.writeCount() != 0 || !reflect.DeepEqual(rep.codes(), []Code{c.want}) {
				t.Fatalf("writes=%v reports=%v", f.writes, rep.codes())
			}
		})
	}
}

func TestTransportFailureIsReturned(t *testing.T) {
	f := chain2()
	f.stacksCode = 500
	e, rep, _ := setup(t, f, res(ModeLink))
	if err := e.Run(context.Background(), job(2)); err == nil {
		t.Fatal("a 500 must be returned so the caller can retry")
	}
	if len(rep.codes()) != 0 || f.writeCount() != 0 {
		t.Fatalf("writes=%v reports=%v", f.writes, rep.codes())
	}
}

func TestDecideReportPassesThroughWithoutWrite(t *testing.T) {
	f := newFakeGH()
	f.pr(2, "b", "ghost") // base branch has no PR and is not the default
	e, rep, _ := setup(t, f, res(ModeLink))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if f.writeCount() != 0 || !reflect.DeepEqual(rep.codes(), []Code{StaleBase}) {
		t.Fatalf("writes=%v reports=%v", f.writes, rep.codes())
	}
}

func TestReadbackMismatchIsReported(t *testing.T) {
	f := chain2()
	f.dropOnWrite = true
	e, rep, _ := setup(t, f, res(ModeLink))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.codes(), []Code{OutcomeReadbackFails}) {
		t.Fatalf("reports = %v", rep.codes())
	}
}

func TestReportsGoOnlyToWorkspacesInReportOrLink(t *testing.T) {
	f := chain2()
	e, rep, _ := setup(t, f, res(ModeReport, WorkspaceMode{"w1", ModeReport}, WorkspaceMode{"w2", ModeOff}, WorkspaceMode{"w3", ModeLink}))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	want := []WorkspaceMode{{"w1", ModeReport}, {"w3", ModeLink}}
	if len(rep.outs) != 1 || !reflect.DeepEqual(rep.outs[0].Workspaces, want) {
		t.Fatalf("outcomes = %+v", rep.outs)
	}
}

func TestNoEligibleWorkspaceMeansNoReport(t *testing.T) {
	f := chain2()
	e, rep, _ := setup(t, f, res(ModeReport, WorkspaceMode{"w2", ModeOff}))
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if len(rep.outs) != 0 {
		t.Fatalf("reports = %+v", rep.outs)
	}
}

func TestMetricCalledPerOutcome(t *testing.T) {
	f := chain2()
	e, _, _ := setup(t, f, res(ModeLink))
	var got []Code
	e.Metric = func(c Code) { got = append(got, c) }
	if err := e.Run(context.Background(), job(2)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []Code{OutcomeCreated}) {
		t.Fatalf("metric = %v", got)
	}
}
