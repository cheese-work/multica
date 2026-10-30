package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A fake CLI written with os.WriteFile and run straight away fails with
// "text file busy" whenever a parallel sibling forks between the open and the
// close: the child inherits the write descriptor and exec sees a file that is
// still open for writing. writeTestExecutable holds syscall.ForkLock to rule
// that out, so no test file may write an executable any other way.
func TestFakeExecutablesGoThroughWriteTestExecutable(t *testing.T) {
	t.Parallel()
	rawExecutable := regexp.MustCompile(`os\.WriteFile\([^\n]*,\s*0o?7[0-7]{2}\)`)
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file, "exec_fixture_") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if rawExecutable.MatchString(line) {
				t.Errorf("%s:%d writes an executable with os.WriteFile; use writeTestExecutable: %s",
					file, i+1, strings.TrimSpace(line))
			}
		}
	}
}
