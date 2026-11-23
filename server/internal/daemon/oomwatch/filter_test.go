package oomwatch

import "testing"

func TestJournalctlArgsExactProductionFilter(t *testing.T) {
	got, err := JournalctlArgs(testBoot, "s=aaaa;i=1;b=x;m=1;t=1;x=1", "1000")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--follow", "--output=json", "--after-cursor=s=aaaa;i=1;b=x;m=1;t=1;x=1",
		"_BOOT_ID=" + testBoot, "_TRANSPORT=kernel",
		"+",
		"_BOOT_ID=" + testBoot, "SYSLOG_IDENTIFIER=multica-containment-hb", "_UID=1000",
	}
	if !sameStrings(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestJournalctlArgsRefusesBadInput(t *testing.T) {
	for _, tc := range []struct{ name, boot, cursor, uid string }{
		{"empty cursor", testBoot, "", "1000"},
		{"boot with dashes", "77e6ee2a-cacf-4538-9e0a-d628f9a2d460", "c", "1000"},
		{"short boot", "77e6ee2a", "c", "1000"},
		{"boot with a match injection", testBoot[:28] + "+_UID", "c", "1000"},
		{"empty uid", testBoot, "c", ""},
		{"non numeric uid", testBoot, "c", "root"},
	} {
		if _, err := JournalctlArgs(tc.boot, tc.cursor, tc.uid); err == nil {
			t.Errorf("%s: want an error", tc.name)
		}
	}
}

func TestNormalizeBootID(t *testing.T) {
	got, err := NormalizeBootID("77E6EE2A-cacf-4538-9e0a-d628f9a2d460\n")
	if err != nil || got != testBoot {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := NormalizeBootID(testBoot); err != nil || got != testBoot {
		t.Fatalf("32-hex form: got %q, %v", got, err)
	}
	for _, bad := range []string{"", "xyz", "77e6ee2a-cacf-4538-9e0a", testBoot + "0"} {
		if _, err := NormalizeBootID(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
