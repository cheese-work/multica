package oomwatch

import "testing"

func TestClassifierClasses(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  []string
	}{
		{"runs leaf is contained",
			[]string{hdr("python3"), memcgCon(svc+"/runs/bomb", 10), kill(10)},
			[]string{"contained"}},
		{"runs aggregate is contained",
			[]string{hdr("python3"), memcgCon(svc+"/runs", 10), kill(10)},
			[]string{"contained"}},
		{"nested path under a run leaf is contained",
			[]string{hdr("python3"), memcgCon(svc+"/runs/a/b/c", 10), kill(10)},
			[]string{"contained"}},
		{"docker scope is foreign_memcg",
			[]string{hdr("buildkit"), memcgCon(docker, 11), kill(11)},
			[]string{"foreign_memcg"}},
		{"another user unit is foreign_memcg",
			[]string{hdr("python3"), memcgCon(otherSvc, 12), kill(12)},
			[]string{"foreign_memcg"}},
		{"a unit whose name only starts like the service is foreign_memcg",
			[]string{hdr("python3"), memcgCon(svc+"-evil", 12), kill(12)},
			[]string{"foreign_memcg"}},
		{"the service cgroup itself holds",
			[]string{hdr("daemon"), memcgCon(svc, 13), kill(13)},
			[]string{"hold:service_outside_runs"}},
		{"daemon leaf holds",
			[]string{hdr("multica"), memcgCon(svc+"/daemon", 14), kill(14)},
			[]string{"hold:service_outside_runs"}},
		{"prep leaf holds",
			[]string{hdr("git"), memcgCon(svc+"/prep/attempt-1", 15), kill(15)},
			[]string{"hold:service_outside_runs"}},
		{".control leaf holds",
			[]string{hdr("hook"), memcgCon(svc+"/.control", 16), kill(16)},
			[]string{"hold:service_outside_runs"}},
		{"a name that only starts like runs holds",
			[]string{hdr("x"), memcgCon(svc+"/runs-evil/a", 17), kill(17)},
			[]string{"hold:service_outside_runs"}},
		{"an ancestor of the service holds",
			[]string{hdr("x"), memcgCon("/user.slice/user-1000.slice/user@1000.service/app.slice", 18), kill(18)},
			[]string{"hold:ancestor_memcg"}},
		{"CONSTRAINT_NONE holds",
			[]string{hdr("python3"), con("CONSTRAINT_NONE", "", "/system.slice/docker-x.scope", "python3", 19), killGlobal(19)},
			[]string{"hold:constraint_not_memcg"}},
		{"CONSTRAINT_CPUSET holds",
			[]string{hdr("python3"), con("CONSTRAINT_CPUSET", "", docker, "python3", 20), killGlobal(20)},
			[]string{"hold:constraint_not_memcg"}},
		{"CONSTRAINT_MEMORY_POLICY holds",
			[]string{hdr("python3"), con("CONSTRAINT_MEMORY_POLICY", "", docker, "python3", 21), killGlobal(21)},
			[]string{"hold:constraint_not_memcg"}},
		{"unknown constraint holds",
			[]string{hdr("python3"), con("CONSTRAINT_FUTURE", "", docker, "python3", 22), killGlobal(22)},
			[]string{"hold:constraint_not_memcg"}},
		{"CONSTRAINT_MEMCG without oom_memcg holds",
			[]string{hdr("python3"), con("CONSTRAINT_MEMCG", "", docker, "python3", 23), kill(23)},
			[]string{"hold:unparsable_record"}},
		{"relative oom_memcg holds",
			[]string{hdr("python3"), memcgCon("runs/x", 24), kill(24)},
			[]string{"hold:unparsable_record"}},
		{"dot-dot in oom_memcg holds",
			[]string{hdr("python3"), memcgCon(svc+"/runs/../daemon", 25), kill(25)},
			[]string{"hold:unparsable_record"}},
		{"unparsable constraint line holds the record",
			[]string{hdr("python3"), "oom-kill:constraint=", kill(26)},
			[]string{"hold:unparsable_record"}},
		{"record with no constraint line holds",
			[]string{kill(27)},
			[]string{"hold:no_constraint_line"}},
		{"header then record with no constraint line holds",
			[]string{hdr("python3"), kill(28)},
			[]string{"hold:no_constraint_line"}},
		{"unparsable record holds",
			[]string{hdr("python3"), memcgCon(svc+"/runs/x", 29), "Memory cgroup out of memory: Killed process abc (python3) total-vm:1kB"},
			[]string{"hold:unparsable_record"}},
		{"comm with commas and parens still joins on the pid",
			[]string{hdr("next-build"), con("CONSTRAINT_MEMCG", docker, docker+"/docker/buildx/x", "next-build (v16,x", 30), "Memory cgroup out of memory: Killed process 30 (next-build (v16,x) total-vm:34378852kB, anon-rss:609432kB, file-rss:8kB, shmem-rss:0kB, UID:0 pgtables:4668kB oom_score_adj:0"},
			[]string{"foreign_memcg"}},
		{"noise lines do not break the join",
			[]string{hdr("python3"), "CPU: 7 UID: 1000 PID: 93693 Comm: python3", "Call Trace:", "memory: usage 32768kB, limit 32768kB, failcnt 18", memcgCon(svc+"/runs/x", 31), kill(31)},
			[]string{"contained"}},
		{"a task-table row naming Killed process is not a record",
			[]string{hdr("python3"), "[  1234]  1000  1234    70651     9748     8149     1599         0   147456        0           200 Killed process 99 (x)", memcgCon(svc+"/runs/x", 32), kill(32)},
			[]string{"contained"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classes(feedAll(NewClassifier(Config{ServiceCgroup: svc}), tc.lines...))
			if !sameStrings(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Join cases of r9 (Sol r8 P1-2): a record is classified only from its own
// constraint line.
func TestClassifierJoin(t *testing.T) {
	c := func() *Classifier { return NewClassifier(Config{ServiceCgroup: svc}) }
	t.Run("constraint line with another pid holds", func(t *testing.T) {
		got := feedAll(c(), hdr("python3"), memcgCon(svc+"/runs/x", 40), kill(41))
		if len(got) != 1 || !got[0].Hold || got[0].Cause != HoldConstraintPIDMiss {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("two records after one constraint line: the second holds", func(t *testing.T) {
		got := feedAll(c(), hdr("python3"), memcgCon(svc+"/runs/x", 42), kill(42), kill(42))
		if len(got) != 2 || got[0].Class != ClassContained || !got[1].Hold || got[1].Cause != HoldConstraintRejoined {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("Sol r8: contained record then a headerless record within 2 s holds the second", func(t *testing.T) {
		got := feedAll(c(), hdr("python3"), memcgCon(svc+"/runs/x", 43), kill(43), kill(44))
		if len(got) != 2 || got[0].Class != ClassContained || !got[1].Hold {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("an invoked-oom-killer line between the constraint line and the record breaks the join", func(t *testing.T) {
		got := feedAll(c(), hdr("a"), memcgCon(svc+"/runs/x", 45), hdr("b"), kill(45))
		if len(got) != 1 || !got[0].Hold || got[0].Cause != HoldNoConstraintLine {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a foreign memcg OOM with its own constraint line right after a contained one", func(t *testing.T) {
		got := classes(feedAll(c(),
			hdr("a"), memcgCon(svc+"/runs/x", 46), kill(46),
			hdr("b"), memcgCon(docker, 47), kill(47)))
		if !sameStrings(got, []string{"contained", "foreign_memcg"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("a group line in a leaf holds at once", func(t *testing.T) {
		got := feedAll(c(), groupLine(svc+"/runs/x"))
		if len(got) != 1 || !got[0].Hold || got[0].Cause != HoldOOMGroup {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a group line in a docker scope holds at once", func(t *testing.T) {
		got := feedAll(c(), groupLine(docker))
		if len(got) != 1 || !got[0].Hold || got[0].Cause != HoldOOMGroup {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a group line between the constraint line and the record also breaks the join", func(t *testing.T) {
		got := feedAll(c(), hdr("a"), memcgCon(svc+"/runs/x", 48), groupLine(svc+"/runs/x"), kill(48))
		if len(got) != 2 || got[0].Cause != HoldOOMGroup || !got[1].Hold {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a verdict carries the constraint, cgroup and pid", func(t *testing.T) {
		vs := feedAll(c(), hdr("a"), memcgCon(docker, 49), kill(49))
		if len(vs) != 1 {
			t.Fatalf("got %+v", vs)
		}
		v := vs[0]
		if v.PID != 49 || v.Constraint != "CONSTRAINT_MEMCG" || v.OOMMemcg != docker || v.Line != kill(49) {
			t.Fatalf("got %+v", v)
		}
	})
}
