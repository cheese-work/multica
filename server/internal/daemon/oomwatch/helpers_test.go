package oomwatch

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testBoot = "77e6ee2acacf45389e0ad628f9a2d460"
	testUID  = "1000"
	svc      = "/user.slice/user-1000.slice/user@1000.service/app.slice/multica.service"
	docker   = "/system.slice/docker-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.scope"
	otherSvc = "/user.slice/user-1000.slice/user@1000.service/app.slice/graphify-refresh.service"
)

var t0 = time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)

// kernel message builders in the real kernel 7.0 formats (see testdata).
func hdr(comm string) string {
	return comm + " invoked oom-killer: gfp_mask=0xcc0(GFP_KERNEL), order=0, oom_score_adj=200"
}

func con(constraint, oomMemcg, taskMemcg, task string, pid int) string {
	s := "oom-kill:constraint=" + constraint + ",nodemask=(null),cpuset=user.slice,mems_allowed=0"
	if oomMemcg != "" {
		s += ",oom_memcg=" + oomMemcg
	}
	return s + ",task_memcg=" + taskMemcg + ",task=" + task + ",pid=" + strconv.Itoa(pid) + ",uid=1000"
}

func memcgCon(oomMemcg string, pid int) string {
	return con("CONSTRAINT_MEMCG", oomMemcg, oomMemcg, "python3", pid)
}

func kill(pid int) string {
	return fmt.Sprintf("Memory cgroup out of memory: Killed process %d (python3) total-vm:282604kB, anon-rss:32596kB, file-rss:6396kB, shmem-rss:0kB, UID:1000 pgtables:144kB oom_score_adj:200", pid)
}

func killGlobal(pid int) string {
	return fmt.Sprintf("Out of memory: Killed process %d (python3) total-vm:434000kB, anon-rss:116036kB, file-rss:48kB, shmem-rss:0kB, UID:1001 pgtables:352kB oom_score_adj:500", pid)
}

func groupLine(g string) string {
	return "Tasks in " + g + " are going to be killed due to memory.oom.group set"
}

func feedAll(c *Classifier, lines ...string) []Verdict {
	var out []Verdict
	for _, l := range lines {
		if v, ok := c.Feed(l); ok {
			out = append(out, v)
		}
	}
	return out
}

func cursorFor(seq int) string {
	return fmt.Sprintf("s=aaaa;i=%x;b=%s;m=%x;t=0;x=0", seq, testBoot, seq*1000)
}

func kernelEntry(seq int, msg string) Entry {
	return Entry{Cursor: cursorFor(seq), BootID: testBoot, Transport: "kernel", Identifier: "kernel", Message: msg}
}

func hbEntry(seq int, nonce string) Entry {
	return Entry{Cursor: cursorFor(seq), BootID: testBoot, Transport: "syslog", Identifier: HeartbeatTag, UID: testUID, Message: Message(nonce)}
}

// harness collects everything a Reader reports.
type harness struct {
	t        *testing.T
	r        *Reader
	mon      *HeartbeatMonitor
	holds    []HoldEvent
	gaps     []GapEvent
	verdicts []Verdict
	ignored  []string
}

func newHarness(t *testing.T, cursor string) *harness { return newHarnessSvc(t, cursor, svc) }

func newHarnessSvc(t *testing.T, cursor, service string) *harness {
	t.Helper()
	h := &harness{t: t, mon: NewHeartbeatMonitor()}
	h.r = NewReader(ReaderConfig{
		Boot: testBoot, UID: testUID, Cursor: cursor,
		Classifier: Config{ServiceCgroup: service},
		Monitor:    h.mon,
		Sinks: Sinks{
			Hold:    func(e HoldEvent) { h.holds = append(h.holds, e) },
			Gap:     func(e GapEvent) { h.gaps = append(h.gaps, e) },
			Verdict: func(v Verdict) { h.verdicts = append(h.verdicts, v) },
			Ignored: func(reason string, _ Entry) { h.ignored = append(h.ignored, reason) },
		},
	})
	return h
}

// feed processes kernel lines as consecutive entries starting at seq.
func (h *harness) feed(seq int, lines ...string) int {
	for _, l := range lines {
		h.r.Process(kernelEntry(seq, l), t0)
		seq++
	}
	return seq
}

func holdCauses(hs []HoldEvent) []HoldCause {
	var c []HoldCause
	for _, h := range hs {
		c = append(c, h.Cause)
	}
	return c
}

func classes(vs []Verdict) []string {
	var c []string
	for _, v := range vs {
		if v.Hold {
			c = append(c, "hold:"+string(v.Cause))
		} else {
			c = append(c, string(v.Class))
		}
	}
	return c
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// loadFixture reads a JSONL fixture, checks its .sha256 file, and assigns the
// test boot and (when missing) synthetic cursors.
func loadFixture(t *testing.T, name string) []Entry {
	t.Helper()
	path := filepath.Join("testdata", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	want, err := os.ReadFile(path + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sum[:]); !strings.HasPrefix(string(want), got) {
		t.Fatalf("%s hash %s does not match %s", name, got, strings.TrimSpace(string(want)))
	}
	var out []Entry
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		e, err := ParseEntry(sc.Bytes())
		if err != nil {
			t.Fatalf("%s line %d: %v", name, len(out)+1, err)
		}
		e.BootID = testBoot
		e.Cursor = cursorFor(len(out) + 1)
		out = append(out, e)
	}
	return out
}
