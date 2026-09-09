package ply

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestStreamSpacingMatchesHistory(t *testing.T) {
	for _, text := range []string{"\n\n", " \n\n", "\n\nHello\n\n", "\n  \n    code\n\n", "Hello  \nworld\n\nParagraph.\n", "\n# Heading\n\n- item\n"} {
		for size := 1; size <= len(text); size++ {
			var live, history bytes.Buffer
			stream := proseStream{W: &live}
			for n := 0; n < len(text); n += size {
				stream.Delta(text[n:min(n+size, len(text))])
			}
			stream.End()
			Renderer{W: &history}.Item(message("assistant", text))
			if live.String() != history.String() {
				t.Fatalf("%q chunk %d: live %q history %q", text, size, live.String(), history.String())
			}
			if strings.TrimSpace(text) == "" && live.Len() != 0 {
				t.Fatalf("blank response rendered: %q", live.String())
			}
		}
	}
}
func TestStatusErasedBeforeOutputAndDisabledOffTTY(t *testing.T) {
	var errout, out bytes.Buffer
	status := &statusLine{W: &errout, Enabled: true}
	status.Show("thinking...")
	status.since = time.Now().Add(-2 * time.Second)
	status.Tick()
	if !strings.Contains(errout.String(), "thinking... 2s") {
		t.Fatal(errout.String())
	}
	prose := proseStream{W: &out, BeforeWrite: status.Clear}
	prose.Delta("\n\n")
	if !status.visible {
		t.Fatal("blank deltas erased thinking")
	}
	prose.Delta("Hello")
	if status.visible || !strings.HasSuffix(errout.String(), "\r\x1b[2K") {
		t.Fatal("status not cleared before prose")
	}
	before := errout.String()
	status.Tick()
	if errout.String() != before {
		t.Fatal("status redrew over prose")
	}
	disabled := &statusLine{W: &errout}
	disabled.Show("thinking...")
	disabled.Tick()
	disabled.Clear()
	if errout.String() != before {
		t.Fatal("status written without tty")
	}
}
