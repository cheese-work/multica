package weblink

import "testing"

func TestBuilders(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"issue", Build("https://app.test/", Issue("ws", "HAN-1")), "https://app.test/ws/issues/HAN-1"},
		{"comment", Build("https://app.test", Comment("ws", "HAN-1", "c1")), "https://app.test/ws/issues/HAN-1#comment-c1"},
		{"chat", Build("https://app.test", Chat("ws", "s1")), "https://app.test/ws/chat?session=s1"},
		{"attachment", Build("https://app.test", Attachment("ws", "a1")), "https://app.test/ws/attachments/a1/preview"},
		{"no origin", Build("", Issue("ws", "HAN-1")), ""},
		{"no slug", Build("https://app.test", Issue("", "HAN-1")), ""},
		{"no identifier", Build("https://app.test", Comment("ws", "", "c1")), ""},
		{"no comment id", Build("https://app.test", Comment("ws", "HAN-1", "")), ""},
		{"slug escaped", Build("https://app.test", Chat("a b", "s1")), "https://app.test/a%20b/chat?session=s1"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}
