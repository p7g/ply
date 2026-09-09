package ply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var binaries string

func TestMain(m *testing.M) {
	dir, e := os.MkdirTemp("", "ply-test-bin-*")
	if e != nil {
		panic(e)
	}
	binaries = dir
	cmd := exec.Command("go", "build", "-o", dir+string(os.PathSeparator), "./cmd/...")
	cmd.Dir = "../.."
	if b, e := cmd.CombinedOutput(); e != nil {
		fmt.Fprintln(os.Stderr, string(b), e)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
func integrationEnv(dir string) []string {
	return replaceEnv(os.Environ(), map[string]string{"XDG_CONFIG_HOME": filepath.Join(dir, "config"), "PATH": binaries + ":" + os.Getenv("PATH"), "PLY_MODEL": "test", "PLY_CONTEXT_WINDOW": "100000", "PLY_APPROVE_COMMAND": "ply-approve-yolo", "PLY_PAGER": "false", "PLY_PROVIDER_RETRIES": "0", "PLY_SYSTEM_FILE": "", "PLY_DETACH": "false", "PLY_COMPACT_AT": "0.8"})
}
func cli(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(binaries, "ply"), args...)
	cmd.Dir = dir
	cmd.Env = integrationEnv(dir)
	b, e := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("command timed out: %s", b)
	}
	return string(b), e
}
func recorded(t *testing.T, dir string, rounds ...[]Item) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("recorded-%d.jsonl", time.Now().UnixNano()))
	var all []Item
	for _, round := range rounds {
		all = append(all, round...)
		all = append(all, Item{"type": "ply.usage", "input_tokens": 10, "output_tokens": 5})
	}
	fixture(t, path, all...)
	return path
}
func callItem(name, id string, args any) Item {
	b, _ := json.Marshal(args)
	return Item{"type": "function_call", "name": name, "id": "fc_" + id, "call_id": id, "arguments": string(b)}
}
func mustItems(t *testing.T, path string) []Item {
	t.Helper()
	items, e := readItems(path, false)
	if e != nil {
		t.Fatal(e)
	}
	return items
}
func TestCLIToolLoopAndClear(t *testing.T) {
	dir := t.TempDir()
	r := recorded(t, dir, []Item{callItem("plan", "p", Item{"text": "1. inspect\n2. fix"}), callItem("bash", "b", BashArgs{Command: "printf 'hello\\n'; pwd", Justification: "inspect", Timeout: 2})}, []Item{message("assistant", "Done.")})
	out, e := cli(t, dir, "--provider", "replay:"+r, "-m", "work", "t.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	items := mustItems(t, filepath.Join(dir, "t.jsonl"))
	if len(pending(items)) != 0 || !strings.Contains(out, "hello") || !strings.Contains(out, "Done.") {
		t.Fatal(out)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "t.jsonl"))
	out, e = cli(t, dir, "--clear", "t.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "t.jsonl"))
	if !bytes.HasPrefix(after, before) {
		t.Fatal("transcript was rewritten")
	}
	replay := Replay(mustItems(t, filepath.Join(dir, "t.jsonl")))
	if len(replay) != 2 || !strings.Contains(textOf(replay[1]), "1. inspect") {
		t.Fatal(replay)
	}
	out, e = cli(t, dir, "--show-plan", "t.jsonl")
	if e != nil || !strings.Contains(out, "2. fix") {
		t.Fatal(out, e)
	}
}
func TestCLIBackgroundWaitAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			dir := t.TempDir()
			command := "sleep 0.3; printf finished"
			if timeout {
				command = "printf before; sleep 10"
			}
			r := recorded(t, dir, []Item{callItem("bash", "bg", BashArgs{Command: command, Justification: "test", Timeout: 1, Background: true})}, []Item{message("assistant", "Waiting.")}, []Item{message("assistant", "Task handled.")})
			out, e := cli(t, dir, "--provider", "replay:"+r, "-m", "run", "t.jsonl")
			if e != nil {
				t.Fatal(out, e)
			}
			done := latest(mustItems(t, filepath.Join(dir, "t.jsonl")), "ply.task_done")
			if done == nil || done["timed_out"] != timeout || !strings.Contains(out, "Task handled.") {
				t.Fatal(done, out)
			}
		})
	}
}
func TestCLIDetachedTaskSurvivesAndEnforcesDeadline(t *testing.T) {
	dir := t.TempDir()
	r := recorded(t, dir, []Item{callItem("bash", "bg", BashArgs{Command: "printf before; sleep 10", Justification: "test", Timeout: 1, Background: true})}, []Item{message("assistant", "Detached.")})
	out, e := cli(t, dir, "--detach", "--provider", "replay:"+r, "-m", "run", "t.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	if latest(mustItems(t, filepath.Join(dir, "t.jsonl")), "ply.task_done") != nil {
		t.Fatal("did not detach")
	}
	time.Sleep(1300 * time.Millisecond)
	out, e = cli(t, dir, "--tasks", "t.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	done := latest(mustItems(t, filepath.Join(dir, "t.jsonl")), "ply.task_done")
	if done == nil || done["timed_out"] != true || !strings.Contains(str(done["output_tail"]), "before") {
		t.Fatal(done, out)
	}
}
func TestCLIDenialAndOutputTruncation(t *testing.T) {
	dir := t.TempDir()
	r := recorded(t, dir, []Item{callItem("bash", "b", BashArgs{Command: "touch should-not-exist", Justification: "test"})}, []Item{message("assistant", "Denied.")})
	out, e := cli(t, dir, "--approve-command", "printf 'not allowed'; exit 1", "--provider", "replay:"+r, "-m", "run", "t.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	if _, e = os.Stat(filepath.Join(dir, "should-not-exist")); !os.IsNotExist(e) {
		t.Fatal("denied command executed")
	}
	if !strings.Contains(out, "DENIED: not allowed") {
		t.Fatal(out)
	}
	r = recorded(t, dir, []Item{callItem("bash", "b", BashArgs{Command: "printf '1\\n2\\n3\\n4\\n5\\n6\\n'", Justification: "test"})}, []Item{message("assistant", "OK.")})
	out, e = cli(t, dir, "--output-max-lines", "2", "--provider", "replay:"+r, "-m", "run", "t.jsonl")
	if e != nil || !strings.Contains(out, "4 lines omitted") {
		t.Fatal(out, e)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, ".ply", "out", "*"))
	if len(paths) != 1 {
		t.Fatal(paths)
	}
	b, _ := os.ReadFile(paths[0])
	if string(b) != "1\n2\n3\n4\n5\n6\n" {
		t.Fatal(string(b))
	}
}
func TestCLISubagentApprovalProxy(t *testing.T) {
	dir := t.TempDir()
	child := recorded(t, dir, []Item{callItem("bash", "child", BashArgs{Command: "printf child-success", Justification: "child inspection"})}, []Item{message("assistant", "Child result.")})
	command := fmt.Sprintf("ply --subagent --provider replay:%s -m child child.jsonl", child)
	parent := recorded(t, dir, []Item{callItem("bash", "parent", BashArgs{Command: command, Justification: "delegate", Timeout: 5, Background: true})}, []Item{message("assistant", "Waiting for child.")}, []Item{message("assistant", "Parent done.")})
	out, e := cli(t, dir, "--provider", "replay:"+parent, "-m", "run", "parent.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	items := mustItems(t, filepath.Join(dir, "parent.jsonl"))
	approval := latest(items, "ply.approval")
	if num(approval["depth"]) != 1 {
		t.Fatal(approval, out)
	}
	done := latest(items, "ply.task_done")
	if !strings.Contains(str(done["output_tail"]), "Child result.") || !strings.Contains(str(done["output_tail"]), "child.jsonl") {
		t.Fatal(done, out)
	}
}
func TestCLISubagentEOF(t *testing.T) {
	dir := t.TempDir()
	r := recorded(t, dir, []Item{callItem("bash", "b", BashArgs{Command: "touch should-not-exist", Justification: "test"})}, []Item{message("assistant", "Wrapped up.")})
	out, e := cli(t, dir, "--subagent", "--provider", "replay:"+r, "-m", "run", "t.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatal("non-JSON protocol output", line)
		}
	}
	i := latest(mustItems(t, filepath.Join(dir, "t.jsonl")), "ply.approval")
	if i["reason"] != "no parent attached" {
		t.Fatal(i, out)
	}
}
func TestCLIModelInterruptRecordsOnlyVisibleText(t *testing.T) {
	dir := t.TempDir()
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"visible text\"}\n\n")
		w.(http.Flusher).Flush()
		close(ready)
		<-r.Context().Done()
	}))
	defer srv.Close()
	cmd := exec.Command(filepath.Join(binaries, "ply"), "--base-url", srv.URL, "-m", "run", "t.jsonl")
	cmd.Dir = dir
	cmd.Env = integrationEnv(dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("no provider call")
	}
	time.Sleep(50 * time.Millisecond)
	cmd.Process.Signal(os.Interrupt)
	e := cmd.Wait()
	if strings.Contains(out.String(), "context canceled") {
		t.Fatal(out.String())
	}
	if ee, ok := e.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
		t.Fatal(out.String(), e)
	}
	items := mustItems(t, filepath.Join(dir, "t.jsonl"))
	i := latest(items, "message")
	if i["ply.partial"] != true || textOf(i) != "visible text" || latest(items, "ply.interrupt") == nil {
		t.Fatal(items, out.String())
	}
}
func TestCLIAutoCompactionPreservesRequestAndPlan(t *testing.T) {
	dir := t.TempDir()
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Item
		json.NewDecoder(r.Body).Decode(&req)
		n := count.Add(1)
		resp := Response{}
		switch n {
		case 1:
			resp.Output = []Item{callItem("plan", "p", Item{"text": "preserve me"})}
			resp.Usage.Input = 90
		case 2:
			if req["tools"] != nil {
				t.Error("compaction had tools")
			}
			b, _ := json.Marshal(req["input"])
			if !strings.Contains(string(b), "original request") {
				t.Error("request lost")
			}
			resp.Output = []Item{message("assistant", "summary of original request")}
			resp.Usage.Output = 5
		case 3:
			b, _ := json.Marshal(req["input"])
			if !strings.Contains(string(b), "preserve me") || !strings.Contains(string(b), "summary of original request") {
				t.Error(string(b))
			}
			resp.Output = []Item{message("assistant", "Done.")}
		default:
			t.Error("unexpected round", n)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	out, e := cli(t, dir, "--base-url", srv.URL, "--context-window", "100", "-m", "original request", "t.jsonl")
	if e != nil || count.Load() != 3 {
		t.Fatal(out, e, count.Load())
	}
	if latest(mustItems(t, filepath.Join(dir, "t.jsonl")), "ply.compaction") == nil {
		t.Fatal(out)
	}
}
func TestCLIApprovalEnvironment(t *testing.T) {
	dir := t.TempDir()
	r := recorded(t, dir, []Item{callItem("bash", "b", BashArgs{Command: "printf hello", Justification: "because", Timeout: 7})}, []Item{message("assistant", "Done.")})
	check := `test "$PLY_COMMAND" = 'printf hello' && test "$PLY_JUSTIFICATION" = because && test "$PLY_USER_MSG" = request && test "$PLY_BACKGROUND" = 0 && test "$PLY_TIMEOUT" = 7 && test "$PLY_APPROVAL_DEPTH" = 0 && test "$PLY_CWD" = "$PWD"`
	out, e := cli(t, dir, "--approve-command", check, "--provider", "replay:"+r, "-m", "request", "t.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	if latest(mustItems(t, filepath.Join(dir, "t.jsonl")), "ply.approval")["result"] != "approved" {
		t.Fatal(out)
	}
}
func TestProcessTimeoutKillsGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	code, _, timed, e := runProcess(ctx, "/bin/bash", "sleep 10 & echo $!; wait", t.TempDir(), &out, nil, 1)
	if e != nil || !timed || code == 0 {
		t.Fatal(code, timed, e)
	}
	var pid int
	fmt.Sscanf(out.String(), "%d", &pid)
	if pid == 0 {
		t.Fatal(out.String())
	} // A dead child may briefly be a zombie until reaped by init.
	if e := syscall.Kill(pid, 0); e != nil && e != syscall.ESRCH {
		t.Fatal(e)
	}
}

func TestCLIApprovalWhileForegroundWaits(t *testing.T) {
	dir := t.TempDir()
	child := recorded(t, dir, []Item{callItem("bash", "child", BashArgs{Command: "touch child-finished", Justification: "finish"})}, []Item{message("assistant", "Child done.")})
	parent := recorded(t, dir, []Item{callItem("bash", "spawn", BashArgs{Command: fmt.Sprintf("ply --subagent --provider replay:%s -m child child.jsonl", child), Justification: "delegate", Timeout: 5, Background: true}), callItem("bash", "wait", BashArgs{Command: "while ! test -f child-finished; do sleep 0.05; done", Justification: "wait", Timeout: 3})}, []Item{message("assistant", "Finished.")}, []Item{message("assistant", "Acknowledged.")})
	out, e := cli(t, dir, "--provider", "replay:"+parent, "-m", "run", "parent.jsonl")
	if e != nil || strings.Contains(out, "TIMED_OUT") {
		t.Fatal(out, e)
	}
	if _, e = os.Stat(filepath.Join(dir, "child-finished")); e != nil {
		t.Fatal(e, out)
	}
}
func TestCLIResumeDoesNotRerunAmbiguousTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	fixture(t, path, callItem("bash", "ambiguous", BashArgs{Command: "touch should-not-exist", Justification: "test"}))
	r := recorded(t, dir, []Item{message("assistant", "Recovered.")})
	out, e := cli(t, dir, "--allow-empty", "--provider", "replay:"+r, "t.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	items := mustItems(t, path)
	if len(pending(items)) != 0 {
		t.Fatal(items)
	}
	if _, e = os.Stat(filepath.Join(dir, "should-not-exist")); !os.IsNotExist(e) {
		t.Fatal("reran ambiguous command")
	}
}
func TestCLINestedSubagentDepth(t *testing.T) {
	dir := t.TempDir()
	leaf := recorded(t, dir, []Item{callItem("bash", "leaf", BashArgs{Command: "printf leaf", Justification: "leaf"})}, []Item{message("assistant", "Leaf result.")})
	middle := recorded(t, dir, []Item{callItem("bash", "middle", BashArgs{Command: fmt.Sprintf("ply --subagent --provider replay:%s -m leaf leaf.jsonl", leaf), Justification: "middle", Timeout: 5, Background: true})}, []Item{message("assistant", "Waiting.")}, []Item{message("assistant", "Middle result.")})
	root := recorded(t, dir, []Item{callItem("bash", "root", BashArgs{Command: fmt.Sprintf("ply --subagent --provider replay:%s -m middle middle.jsonl", middle), Justification: "root", Timeout: 7, Background: true})}, []Item{message("assistant", "Waiting.")}, []Item{message("assistant", "Root result.")})
	out, e := cli(t, dir, "--provider", "replay:"+root, "-m", "run", "root.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	items := mustItems(t, filepath.Join(dir, "root.jsonl"))
	maxDepth := 0
	for _, i := range items {
		if str(i["type"]) == "ply.approval" {
			maxDepth = max(maxDepth, num(i["depth"]))
		}
	}
	if maxDepth != 2 {
		t.Fatal(maxDepth, out)
	}
}

func TestCLIBackgroundStdinIsDevNull(t *testing.T) {
	dir := t.TempDir()
	r := recorded(t, dir, []Item{callItem("bash", "bg", BashArgs{Command: "cat; printf eof", Justification: "test stdin", Timeout: 2, Background: true})}, []Item{message("assistant", "Waiting.")}, []Item{message("assistant", "Done.")})
	out, e := cli(t, dir, "--provider", "replay:"+r, "-m", "run", "t.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	done := latest(mustItems(t, filepath.Join(dir, "t.jsonl")), "ply.task_done")
	if done == nil || done["timed_out"] != false || str(done["output_tail"]) != "eof" {
		t.Fatal(done, out)
	}
}
func TestCLIDetachedSubagentDeniesFutureApproval(t *testing.T) {
	dir := t.TempDir()
	child := recorded(t, dir, []Item{callItem("bash", "child", BashArgs{Command: "touch should-not-exist", Justification: "test"})}, []Item{message("assistant", "Denied safely.")})
	parent := recorded(t, dir, []Item{callItem("bash", "parent", BashArgs{Command: fmt.Sprintf("sleep 0.4; ply --subagent --provider replay:%s -m child child.jsonl", child), Justification: "delegate", Timeout: 4, Background: true})}, []Item{message("assistant", "Detached.")})
	start := time.Now()
	out, e := cli(t, dir, "--detach", "--provider", "replay:"+parent, "-m", "run", "parent.jsonl")
	if e != nil {
		t.Fatal(out, e)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("detach held output pipes open", out)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		items, e := readItems(filepath.Join(dir, "child.jsonl"), true)
		if e == nil && latest(items, "ply.approval") != nil {
			if latest(items, "ply.approval")["reason"] != "no parent attached" {
				t.Fatal(items)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not receive EOF")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, e = os.Stat(filepath.Join(dir, "should-not-exist")); !os.IsNotExist(e) {
		t.Fatal("detached child executed denied command")
	}
}
func TestCLIForegroundInterrupt(t *testing.T) {
	dir := t.TempDir()
	r := recorded(t, dir, []Item{callItem("bash", "fg", BashArgs{Command: "printf partial; touch running; sleep 10", Justification: "test", Timeout: 20})})
	cmd := exec.Command(filepath.Join(binaries, "ply"), "--provider", "replay:"+r, "-m", "run", "t.jsonl")
	cmd.Dir = dir
	cmd.Env = integrationEnv(dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(dir, "running")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("command did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cmd.Process.Signal(os.Interrupt)
	e := cmd.Wait()
	if strings.Contains(out.String(), "context canceled") {
		t.Fatal(out.String())
	}
	if ee, ok := e.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
		t.Fatal(out.String(), e)
	}
	items := mustItems(t, filepath.Join(dir, "t.jsonl"))
	output := str(latest(items, "function_call_output")["output"])
	if !strings.Contains(output, "partial") || !strings.Contains(output, "INTERRUPTED") || latest(items, "ply.interrupt")["during"] != "command" {
		t.Fatal(items)
	}
}

func TestCLISubagentSteersForeground(t *testing.T) {
	dir := t.TempDir()
	r := recorded(t, dir, []Item{callItem("bash", "fg", BashArgs{Command: "touch running; sleep 10", Justification: "test", Timeout: 20})}, []Item{message("assistant", "Changed direction.")})
	cmd := exec.Command(filepath.Join(binaries, "ply"), "--subagent", "--provider", "replay:"+r, "-m", "run", "t.jsonl")
	cmd.Dir = dir
	cmd.Env = integrationEnv(dir)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer in.Close()
	dec := json.NewDecoder(out)
	for {
		var ev Item
		if e := dec.Decode(&ev); e != nil {
			cmd.Process.Kill()
			t.Fatal(e)
		}
		if ev["event"] == "approval_request" {
			json.NewEncoder(in).Encode(Item{"cmd": "approve", "id": ev["id"], "ok": true})
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(dir, "running")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("foreground never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	json.NewEncoder(in).Encode(Item{"cmd": "steer", "message": "change direction"})
	for {
		var ev Item
		if e := dec.Decode(&ev); e != nil {
			t.Fatal(e)
		}
		if ev["event"] == "result" {
			if ev["text"] != "Changed direction." {
				t.Fatal(ev)
			}
			break
		}
	}
	if e := cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	items := mustItems(t, filepath.Join(dir, "t.jsonl"))
	if latest(items, "ply.interrupt") == nil || !strings.Contains(str(latest(items, "function_call_output")["output"]), "INTERRUPTED") {
		t.Fatal(items)
	}
	found := false
	for _, i := range items {
		if str(i["role"]) == "user" && textOf(i) == "change direction" {
			found = true
		}
	}
	if !found {
		t.Fatal("steering message not persisted")
	}
}
func TestCLIConfigOnlyRecordedWhenChanged(t *testing.T) {
	dir := t.TempDir()
	r := recorded(t, dir, []Item{message("assistant", "Done.")})
	for n := 0; n < 2; n++ {
		out, e := cli(t, dir, "--provider", "replay:"+r, "-m", "run", "t.jsonl")
		if e != nil {
			t.Fatal(out, e)
		}
	}
	count := 0
	for _, i := range mustItems(t, filepath.Join(dir, "t.jsonl")) {
		if str(i["type"]) == "ply.config" {
			count++
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
}
