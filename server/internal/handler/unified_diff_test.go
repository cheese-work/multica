package handler

import "testing"

func TestUnifiedDiff(t *testing.T) {
	cases := []struct {
		name, a, b, want string
	}{
		{"identical", "a\nb", "a\nb", ""},
		{"replace middle", "a\nb\nc", "a\nX\nc", "--- from\n+++ to\n@@ -1,3 +1,3 @@\n a\n-b\n+X\n c\n"},
		{"empty to text", "", "x\ny", "--- from\n+++ to\n@@ -0,0 +1,2 @@\n+x\n+y\n"},
		{"text to empty", "x", "", "--- from\n+++ to\n@@ -1,1 +0,0 @@\n-x\n"},
		{
			"two far-apart changes make two hunks",
			"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12",
			"1\nA\n3\n4\n5\n6\n7\n8\n9\n10\n11\nB",
			"--- from\n+++ to\n@@ -1,5 +1,5 @@\n 1\n-2\n+A\n 3\n 4\n 5\n@@ -9,4 +9,4 @@\n 9\n 10\n 11\n-12\n+B\n",
		},
	}
	for _, c := range cases {
		got, err := unifiedDiff(c.a, c.b, "from", "to")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestUnifiedDiffRefusesHugeInput(t *testing.T) {
	big := make([]byte, 0, 6000*2)
	for i := 0; i < 6000; i++ {
		big = append(big, 'a', '\n')
	}
	a := string(big)
	b := string(big) + "z\n"
	// Different middle sections of 6000 lines each exceed the cell cap.
	c := ""
	for i := 0; i < 6000; i++ {
		c += "b\n"
	}
	if _, err := unifiedDiff(a, c, "from", "to"); err == nil {
		t.Fatal("want error for oversize diff")
	}
	if _, err := unifiedDiff(a, b, "from", "to"); err != nil {
		t.Fatalf("shared prefix must trim before the cap: %v", err)
	}
}
