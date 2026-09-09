package ply

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type Renderer struct {
	W                      io.Writer
	Quiet, Thinking, Color bool
}

func indent(s, p string) string {
	return p + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n"+p)
}
func (r Renderer) Item(i Item) {
	typ := str(i["type"])
	s := ""
	dim := false
	if r.Quiet && (typ != "message" || str(i["role"]) != "assistant") {
		return
	}
	switch typ {
	case "message":
		switch str(i["role"]) {
		case "user":
			s = indent(textOf(i), "> ")
		case "assistant":
			s = textOf(i)
		}
	case "reasoning":
		if r.Thinking {
			s = indent(textOf(i), "~ ")
			dim = true
		}
	case "function_call":
		if str(i["name"]) == "bash" {
			var a BashArgs
			if decodeArgs(i, &a) == nil {
				s = "$ " + a.Command
				if a.Timeout > 0 {
					s += fmt.Sprintf("  [timeout %ds]", a.Timeout)
				}
				if a.Background {
					s += " &"
				}
			}
		}
	case "function_call_output":
		s = indent(str(i["output"]), "  ")
		dim = true
	case "ply.plan":
		s = "[plan updated]"
		dim = true
	case "ply.task_started":
		s = fmt.Sprintf("[task %v started]", i["task"])
		dim = true
	case "ply.task_done":
		s = fmt.Sprintf("  [task %v done, exit %v, %vs]\n%s", i["task"], i["exit_code"], i["duration_s"], indent(str(i["output_tail"]), "  "))
		dim = true
	case "ply.compaction":
		if i["summary"] == nil {
			s = "[cleared]"
		} else {
			s = fmt.Sprintf("[compacted: %v → %v tokens]", i["tokens_before"], i["tokens_after"])
		}
		dim = true
	case "ply.interrupt":
		s = "[interrupted]"
		dim = true
	case "ply.error":
		s = "[error: " + strings.ReplaceAll(str(i["message"]), "\n", " ") + "]"
		dim = true
	}
	if s != "" {
		if dim && r.Color {
			s = "\x1b[2m" + s + "\x1b[0m"
		}
		fmt.Fprint(r.W, s, "\n\n")
	}
}
func visible(items []Item, all bool, tail int) []Item {
	start := 0
	if !all {
		for n, i := range items {
			if str(i["type"]) == "ply.compaction" {
				start = n
			}
		}
	}
	if tail >= 0 && len(items)-start > tail {
		start = len(items) - tail
	}
	return items[start:]
}
func page(s string, c Config) {
	if tty(os.Stdout) && c.B("pager") {
		p := os.Getenv("PAGER")
		if p == "" {
			p = "less -FRX"
		}
		cmd := exec.Command("/bin/sh", "-c", p)
		cmd.Stdin = strings.NewReader(s)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if cmd.Run() == nil {
			return
		}
	}
	fmt.Print(s)
}
