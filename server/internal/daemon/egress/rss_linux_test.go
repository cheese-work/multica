//go:build linux

package egress

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func rssBytes(t *testing.T) int64 {
	t.Helper()
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			kb, _ := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
			return kb << 10
		}
	}
	t.Fatal("VmRSS not found")
	return 0
}

// TestM3AddedRSS is the egress-proxy part of M3: 20 concurrent runs, each with
// its own proxy and 5 live tunnels, add at most 100 MB. The clients and the
// origin run in this process too, so the figure is an upper bound.
func TestM3AddedRSS(t *testing.T) {
	const runs, tunnels, limit = 20, 5, 100 << 20
	port := origin(t)
	runtime.GC()
	before := rssBytes(t)
	for i := 0; i < runs; i++ {
		dial := startProxy(t, &Proxy{AllowLoopback: true})
		for j := 0; j < tunnels; j++ {
			c := dial()
			if code, br := status(t, c, connect("127.0.0.1", port)); code != 200 {
				t.Fatalf("CONNECT status %d", code)
			} else {
				io.WriteString(c, "x\n")
				br.ReadString('\n')
			}
		}
	}
	after := rssBytes(t)
	t.Logf("M3 egress: RSS before=%d after=%d added=%d bytes (%d runs x %d tunnels)", before, after, after-before, runs, tunnels)
	if added := after - before; added > limit {
		t.Fatalf("added RSS %d > %d", added, limit)
	}
}
