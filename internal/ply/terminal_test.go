package ply

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTerminalApprovalAndStreaming(t *testing.T) {
	python, e := exec.LookPath("python3")
	if e != nil {
		t.Skip("terminal regression requires Python 3's standard-library pty module")
	}
	driver, e := filepath.Abs("../../testdata/terminal_driver.py")
	if e != nil {
		t.Fatal(e)
	}
	for _, reply := range []string{"y", "n"} {
		t.Run(reply, func(t *testing.T) {
			dir := t.TempDir()
			var rounds atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := rounds.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.(http.Flusher).Flush()
				if n == 1 {
					time.Sleep(1100 * time.Millisecond)
				}
				text := "\n\n"
				output := []Item{message("assistant", text), callItem("bash", "interactive", BashArgs{Command: "printf APPROVED_EXECUTED", Justification: "test interactive approval"})}
				if n > 1 {
					text = "\n\nDONE\n\n"
					output = []Item{message("assistant", text)}
				}
				for _, delta := range strings.SplitAfter(text, "\n") {
					b, _ := json.Marshal(Item{"type": "response.output_text.delta", "delta": delta})
					fmt.Fprintf(w, "data: %s\n\n", b)
					w.(http.Flusher).Flush()
				}
				b, _ := json.Marshal(Item{"type": "response.completed", "response": Item{"output": output, "usage": Item{"input_tokens": 10, "output_tokens": 2}}})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}))
			defer srv.Close()
			cmd := exec.Command(python, driver, filepath.Join(binaries, "ply"), "--base-url", srv.URL, "--approve-command", "ply-approve-chain ply-approve-allowlist ply-approve-ask", "-m", "run", "t.jsonl")
			cmd.Dir = dir
			cmd.Env = replaceEnv(integrationEnv(dir), map[string]string{"PLY_TEST_REPLY": reply})
			b, e := cmd.CombinedOutput()
			if e != nil {
				t.Fatalf("PTY driver: %v\n%s", e, b)
			}
			var result struct {
				Output   string
				Answered bool
				Exit     *int
			}
			if e = json.Unmarshal(b, &result); e != nil {
				t.Fatal(e, string(b))
			}
			out := strings.ReplaceAll(result.Output, "\r\n", "\n")
			if !result.Answered || result.Exit == nil || *result.Exit != 0 {
				t.Fatalf("approval stalled or failed: %s", b)
			}
			if strings.Count(out, "printf APPROVED_EXECUTED") != 1 {
				t.Fatalf("duplicate command: %q", out)
			}
			if strings.Contains(out, "test interactive approval") {
				t.Fatalf("justification duplicated: %q", out)
			}
			if strings.Contains(out, "\n\n\n") {
				t.Fatalf("extra blank lines: %q", out)
			}
			if !strings.Contains(out, "thinking... 1s") || !strings.Contains(out, "\r\x1b[2K$") {
				t.Fatalf("missing/uncleared thinking state: %q", out)
			}
			if !strings.Contains(out, "DONE\n\n") {
				t.Fatalf("no continued model round: %q", out)
			}
			items := mustItems(t, filepath.Join(dir, "t.jsonl"))
			approval := latest(items, "ply.approval")
			want := "approved"
			if reply == "n" {
				want = "denied"
			}
			if approval["result"] != want {
				t.Fatal(approval)
			}
			tool := str(latest(items, "function_call_output")["output"])
			if reply == "y" && !strings.Contains(tool, "APPROVED_EXECUTED") {
				t.Fatal(tool)
			}
			if reply == "n" && !strings.HasPrefix(tool, "DENIED:") {
				t.Fatal(tool)
			}
		})
	}
}

func TestTerminalCompactionStatus(t *testing.T) {
	python, e := exec.LookPath("python3")
	if e != nil {
		t.Skip("requires Python PTY support")
	}
	driver, e := filepath.Abs("../../testdata/terminal_driver.py")
	if e != nil {
		t.Fatal(e)
	}
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprint(quiet), func(t *testing.T) {
			dir := t.TempDir()
			fixture(t, filepath.Join(dir, "t.jsonl"), message("user", "summarize this"))
			record := recorded(t, dir, []Item{message("assistant", "Compact summary")})
			args := []string{driver, filepath.Join(binaries, "ply"), "--compact", "--provider", "replay:" + record, "t.jsonl"}
			if quiet {
				args = append(args, "-q")
			}
			cmd := exec.Command(python, args...)
			cmd.Dir = dir
			cmd.Env = integrationEnv(dir)
			b, e := cmd.CombinedOutput()
			if e != nil {
				t.Fatal(e, string(b))
			}
			var result struct {
				Output string
				Exit   *int
			}
			if e = json.Unmarshal(b, &result); e != nil {
				t.Fatal(e, string(b))
			}
			if result.Exit == nil || *result.Exit != 0 {
				t.Fatal(string(b))
			}
			if strings.Contains(result.Output, "thinking...") || strings.Contains(result.Output, "compacting...") == quiet {
				t.Fatal(result.Output)
			}
		})
	}
}
