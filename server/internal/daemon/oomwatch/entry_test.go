package oomwatch

import "testing"

func TestParseEntry(t *testing.T) {
	line := `{"__CURSOR":"s=a;i=1f;b=` + testBoot + `;m=3e8;t=5;x=9","_BOOT_ID":"` + testBoot + `","_TRANSPORT":"kernel","SYSLOG_IDENTIFIER":"kernel","MESSAGE":"hello","_SOURCE_MONOTONIC_TIMESTAMP":"123","__MONOTONIC_TIMESTAMP":"456"}`
	e, err := ParseEntry([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if e.Cursor == "" || e.BootID != testBoot || e.Transport != "kernel" || e.Identifier != "kernel" || e.Message != "hello" || e.SourceMono != "123" || e.Mono != "456" || e.UID != "" {
		t.Fatalf("got %+v", e)
	}
}

func TestParseEntryUIDAndBinaryMessage(t *testing.T) {
	e, err := ParseEntry([]byte(`{"_UID":"1000","MESSAGE":[104,105,255]}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.UID != "1000" || e.Message != "hi\xff" {
		t.Fatalf("got %+v", e)
	}
}

func TestParseEntryRejectsGarbage(t *testing.T) {
	for _, bad := range []string{``, `not json`, `[1,2]`, `{"MESSAGE":`, `{"MESSAGE":{"a":1}}`} {
		if _, err := ParseEntry([]byte(bad)); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestParseCursorAndOrder(t *testing.T) {
	a, err := ParseCursor("s=aa;i=10;b=bb;m=64;t=1;x=1")
	if err != nil || a.SeqnumID != "aa" || a.Seqnum != 16 || a.Boot != "bb" || a.Mono != 100 {
		t.Fatalf("got %+v, %v", a, err)
	}
	b, _ := ParseCursor("s=aa;i=11;b=bb;m=65;t=1;x=1")
	if before, ok := AtOrBefore(a, b); !before || !ok {
		t.Error("lower seqnum is before")
	}
	if before, ok := AtOrBefore(b, a); before || !ok {
		t.Error("higher seqnum is after")
	}
	if before, ok := AtOrBefore(a, a); !before || !ok {
		t.Error("a cursor is at or before itself")
	}
	// another journal file (new seqnum id) in the same boot: order by monotonic time
	c, _ := ParseCursor("s=cc;i=1;b=bb;m=c8;t=1;x=1")
	if before, ok := AtOrBefore(a, c); !before || !ok {
		t.Error("same boot, later monotonic: a is before c")
	}
	if before, ok := AtOrBefore(c, a); before || !ok {
		t.Error("same boot, c is after a")
	}
	d, _ := ParseCursor("s=aa;i=1;b=other;m=1;t=1;x=1")
	if _, ok := AtOrBefore(d, a); ok {
		t.Error("different boots are not comparable")
	}
	for _, bad := range []string{"", "x", "s=aa;i=zz;b=bb;m=1", "s=aa;b=bb;m=1;i=1;i=2"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
