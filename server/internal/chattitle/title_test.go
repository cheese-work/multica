package chattitle

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const viFirstMessage = "tôi muốn adopt ý tưởng này vào hệ thống chat của chúng ta để tiêu đề cuộc trò chuyện hiển thị đầy đủ hơn nhiều"

func TestDeriveCutsAtWordBoundaryWithoutEllipsis(t *testing.T) {
	if utf8.RuneCountInString(viFirstMessage) <= 80 {
		t.Fatal("fixture must exceed 80 code points")
	}
	got := Derive(viFirstMessage)
	if strings.Contains(got, "…") || strings.HasSuffix(got, "...") {
		t.Fatalf("stored title carries an ellipsis: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > 80 || n < 60 {
		t.Fatalf("title has %d code points, want 60..80: %q", n, got)
	}
	if !strings.HasPrefix(viFirstMessage, got+" ") {
		t.Fatalf("title %q is not a whole-word prefix of the message", got)
	}
}

func TestDeriveEmojiNeverSplits(t *testing.T) {
	got := Derive(strings.Repeat("😀", 100))
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 80 {
		t.Fatalf("got %d code points, valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
}

func TestDeriveKeepsShortAndFirstLine(t *testing.T) {
	if got := Derive("\n# Hello  world\nsecond line"); got != "Hello world" {
		t.Fatalf("got %q", got)
	}
	if got := Derive(strings.Repeat("a", 80)); got != strings.Repeat("a", 80) {
		t.Fatalf("80-char title was altered: %q", got)
	}
}

// A long URL right after a short prefix must not be dropped whole: the word
// boundary only applies when it keeps a reasonable part of the limit.
func TestDeriveLongURLHardCutsInsteadOfDroppingIt(t *testing.T) {
	msg := "tôi muốn adopt ý tưởng này https://github.com/cheese-work/multica/blob/main/docs/some/very/long/path/README.md"
	got := Derive(msg)
	if n := utf8.RuneCountInString(got); n != 80 {
		t.Fatalf("title has %d code points, want 80: %q", n, got)
	}
	if !strings.HasPrefix(msg, got) {
		t.Fatalf("title %q is not a prefix of the message", got)
	}
}
