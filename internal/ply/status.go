package ply

import (
	"fmt"
	"io"
	"time"
)

// Status is ephemeral, stderr-only, and controlled by the turn's event loop.
// No background writer can race with streaming output or an approval prompt.
type statusLine struct {
	W       io.Writer
	Enabled bool
	text    string
	since   time.Time
	visible bool
}

func (s *statusLine) Show(text string) {
	if s == nil || !s.Enabled {
		return
	}
	s.text = text
	s.since = time.Now()
	s.draw(text)
}
func (s *statusLine) Tick() {
	if s == nil || !s.Enabled || !s.visible {
		return
	}
	s.draw(fmt.Sprintf("%s %ds", s.text, int(time.Since(s.since).Seconds())))
}
func (s *statusLine) draw(text string) {
	if s.visible {
		fmt.Fprint(s.W, "\r\x1b[2K")
	}
	fmt.Fprint(s.W, text)
	s.visible = true
}
func (s *statusLine) Clear() {
	if s == nil || !s.visible {
		return
	}
	fmt.Fprint(s.W, "\r\x1b[2K")
	s.visible = false
}
