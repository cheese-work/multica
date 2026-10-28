package util

import (
	"reflect"
	"testing"
)

func TestParseMentionsRetainsForkCodeExamples(t *testing.T) {
	const mention = "[@Agent](mention://agent/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa)"
	want := []Mention{{Type: "agent", ID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}}
	for _, content := range []string{"`" + mention + "`", "```text\n" + mention + "\n```"} {
		if got := ParseMentions(content); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseMentions(%q) = %#v, want %#v until the code-mention policy is decided", content, got, want)
		}
	}
}
