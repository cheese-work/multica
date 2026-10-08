package buildinfo

import "testing"

func TestSplit(test *testing.T) {
	for _, testcase := range []struct {
		input string
		plain string
		build string
	}{
		{"0.6.0-20261008-1331", "0.6.0", "20261008-1331"},
		{"v0.6.0-20261008-1331+metadata", "v0.6.0", "20261008-1331"},
		{"0.6.0-rc1-20261008-1331", "0.6.0-rc1", "20261008-1331"},
		{"0.6.0+metadata", "0.6.0", ""},
		{"0.6.0", "0.6.0", ""},
		{"dev", "dev", ""},
		{"v0.6.0-12-gabc1234", "v0.6.0-12-gabc1234", ""},
		{"v0.6.0-dirty", "v0.6.0-dirty", ""},
	} {
		plain, build := Split(testcase.input)
		if plain != testcase.plain || build != testcase.build {
			test.Errorf("Split(%q) = (%q, %q), want (%q, %q)", testcase.input, plain, build, testcase.plain, testcase.build)
		}
	}
}
