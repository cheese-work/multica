package incident

import (
	"strings"
	"testing"
	"time"
)

func TestText(t *testing.T) {
	in := Detect(ccRuns(), t0.Add(10*time.Minute))[0]
	if got := Title(in.Class); !strings.Contains(got, ClassConcurrency) {
		t.Errorf("title %q lacks the class", got)
	}
	body := Body(in)
	for _, want := range []string{ccErr, "3 failed runs", "3 issues", "3 agents", "mention://issue/a"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	dm := DMText(in, "[CHE-1](mention://issue/x)")
	if !strings.Contains(dm, "[CHE-1](mention://issue/x)") || !strings.Contains(dm, ClassConcurrency) {
		t.Errorf("dm lacks the link or class: %s", dm)
	}
	for _, s := range []string{body, dm, CloseBody(in.Class, t0)} {
		if strings.Contains(s, "mention://agent") || strings.Contains(s, "mention://member") {
			t.Errorf("text would wake someone: %s", s)
		}
	}
	if got := CloseBody(in.Class, t0); !strings.Contains(got, "Retried: none") {
		t.Errorf("close text must say what was retried: %s", got)
	}
}
