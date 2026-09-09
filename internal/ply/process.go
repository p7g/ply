package ply

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type BashArgs struct {
	Command       string `json:"command"`
	Justification string `json:"justification"`
	Timeout       int    `json:"timeout_s"`
	Background    bool   `json:"background"`
}

func decodeArgs(i Item, v any) error { return json.Unmarshal([]byte(str(i["arguments"])), v) }
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "call"
	}
	return b.String()
}
func truncate(s string, n int, path string, tail bool) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	if tail {
		return strings.Join(lines[len(lines)-n:], "\n")
	}
	head := (n + 1) / 2
	return strings.Join(lines[:head], "\n") + fmt.Sprintf("\n[… %d lines omitted; full output: %s]\n", len(lines)-n, path) + strings.Join(lines[len(lines)-(n-head):], "\n")
}
func runProcess(ctx context.Context, shell, command, cwd string, out io.Writer, stdin io.Reader, timeout int, bridge ...*os.File) (int, float64, bool, error) {
	if e := ctx.Err(); e != nil {
		return -1, 0, false, e
	}
	start := time.Now()
	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = cwd
	cmd.Stdin = stdin
	cmd.Env = replaceEnv(os.Environ(), map[string]string{"PLY_PARENT_INPUT_FD": ""})
	if len(bridge) > 0 {
		cmd.ExtraFiles = bridge
		cmd.Env = replaceEnv(os.Environ(), map[string]string{"PLY_PARENT_INPUT_FD": "3"})
	}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e := cmd.Start(); e != nil {
		return -1, 0, false, e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()
	timed := false
	var e error
	select {
	case e = <-done:
	case <-timer.C:
		timed = true
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		e = <-done
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		e = <-done
	}
	// A shell must not leave untracked descendants holding its output pipes.
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	code := 0
	if e != nil {
		code = -1
		if ee, ok := e.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	return code, time.Since(start).Seconds(), timed, nil
}

type Task struct {
	ID              string    `json:"task"`
	Transcript      string    `json:"transcript"`
	For             int       `json:"for"`
	PID             int       `json:"pid"`
	Command         string    `json:"command"`
	Cwd             string    `json:"cwd"`
	Shell           string    `json:"shell"`
	Log             string    `json:"log"`
	Deadline        time.Time `json:"deadline"`
	Started         time.Time `json:"started"`
	Timeout         int       `json:"timeout_s"`
	Done            bool      `json:"done"`
	Exit            int       `json:"exit_code"`
	Duration        float64   `json:"duration_s"`
	TimedOut        bool      `json:"timed_out"`
	Result          string    `json:"result"`
	ChildTranscript string    `json:"child_transcript"`
	Subagent        bool      `json:"subagent"`
}

func writeJSON(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".state-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(name, path)
}

// Worker is a detached supervisor. It owns the command deadline and durable result.
func Worker(path string) int {
	b, e := os.ReadFile(path)
	if e != nil {
		return 1
	}
	var t Task
	if json.Unmarshal(b, &t) != nil {
		return 1
	}
	t.PID = os.Getpid()
	if writeJSON(path, t) != nil {
		return 1
	}
	log, e := os.OpenFile(t.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return 1
	}
	defer log.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	watchSignals(sig)
	signal.Ignore(syscall.SIGPIPE)
	go func() { <-sig; cancel() }()
	r, w, e := os.Pipe()
	if e != nil {
		return 1
	}
	parsed := make(chan struct{})
	go func() {
		defer close(parsed)
		br := bufio.NewReader(io.TeeReader(r, log))
		for {
			line, e := br.ReadString('\n')
			if len(line) > 0 {
				var ev Item
				if json.Unmarshal([]byte(line), &ev) == nil {
					switch str(ev["event"]) {
					case "hello":
						t.Subagent = true
						t.ChildTranscript = str(ev["transcript"])
					case "result":
						t.Result = str(ev["text"])
					}
					if str(ev["event"]) != "" {
						os.Stdout.Write([]byte(line))
					}
				}
			}
			if e != nil {
				return
			}
		}
	}()
	code, duration, timed, runErr := runProcess(ctx, t.Shell, t.Command, t.Cwd, w, nil, t.Timeout, os.Stdin)
	w.Close()
	<-parsed
	r.Close()
	t.Done = true
	t.Exit = code
	t.Duration = duration
	t.TimedOut = timed
	if runErr != nil {
		io.WriteString(log, runErr.Error())
	}
	log.Sync()
	if writeJSON(path, t) != nil {
		return 1
	}
	return 0
}
func (a *App) startTask(call Item, args BashArgs) (string, error) {
	id := fmt.Sprintf("%d-%d-%d", os.Getpid(), time.Now().UnixNano(), num(call["seq"]))
	dir := filepath.Join(a.Config.Project, "tasks")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	path := filepath.Join(dir, id+".json")
	t := Task{ID: id, Transcript: a.Path, For: num(call["seq"]), Command: args.Command, Cwd: a.Cwd, Shell: a.Config.S("bash.shell"), Log: filepath.Join(dir, id+".log"), Started: time.Now(), Timeout: args.Timeout}
	t.Deadline = t.Started.Add(time.Duration(t.Timeout) * time.Second)
	if e := writeJSON(path, t); e != nil {
		return "", e
	}
	exe, e := os.Executable()
	if e != nil {
		return "", e
	}
	cmd := exec.Command(exe, "--internal-worker", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	in, e := cmd.StdinPipe()
	if e != nil {
		return "", e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return "", e
	}
	errLog, e := os.OpenFile(t.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return "", e
	}
	cmd.Stderr = errLog
	e = cmd.Start()
	errLog.Close()
	if e != nil {
		return "", e
	}
	t.PID = cmd.Process.Pid
	a.Children = append(a.Children, in)
	go func() {
		br := bufio.NewReader(out)
		for {
			line, e := br.ReadBytes('\n')
			if len(line) > 0 {
				var ev Item
				if json.Unmarshal(line, &ev) == nil && str(ev["event"]) == "approval_request" {
					select {
					case a.Proxy <- proxyRequest{ev, in, call["seq"]}:
					case <-a.Context.Done():
						return
					}
				}
			}
			if e != nil {
				break
			}
		}
		cmd.Wait()
	}()
	if e = a.T.Append(Item{"type": "ply.task_started", "task": id, "for": t.For, "pid": t.PID, "command": t.Command, "log": t.Log, "deadline": t.Deadline.Format(time.RFC3339), "subagent": strings.Contains(args.Command, "--subagent")}); e != nil {
		return "", e
	}
	b, _ := json.Marshal(Item{"task_id": id, "pid": t.PID, "log": t.Log})
	return string(b), nil
}
func (a *App) scanTasks() (int, int, error) {
	done := map[string]bool{}
	started := []Item{}
	for _, i := range a.T.Items {
		if str(i["type"]) == "ply.task_done" {
			done[str(i["task"])] = true
		}
		if str(i["type"]) == "ply.task_started" {
			started = append(started, i)
		}
	}
	running, completed := 0, 0
	for _, item := range started {
		id := str(item["task"])
		if done[id] {
			continue
		}
		path := strings.TrimSuffix(str(item["log"]), ".log") + ".json"
		b, e := os.ReadFile(path)
		if e != nil {
			return running, completed, e
		}
		var t Task
		if e = json.Unmarshal(b, &t); e != nil {
			return running, completed, e
		}
		if !t.Done {
			pid := t.PID
			if pid == 0 {
				pid = num(item["pid"])
			}
			if syscall.Kill(pid, 0) == syscall.ESRCH {
				t.Done = true
				t.Exit = -1
				t.Duration = time.Since(t.Started).Seconds()
				t.Result = "[task supervisor exited without a result]"
			} else {
				running++
				continue
			}
		}
		out, _ := os.ReadFile(t.Log)
		tail := truncate(string(out), a.Config.N("output.max_lines"), t.Log, true)
		if t.Result != "" {
			tail = t.Result
			if t.ChildTranscript != "" {
				tail += "\nTranscript: " + t.ChildTranscript
			}
		}
		if e = a.T.Append(Item{"type": "ply.task_done", "task": id, "exit_code": t.Exit, "duration_s": t.Duration, "timed_out": t.TimedOut, "output_tail": tail}); e != nil {
			return running, completed, e
		}
		completed++
	}
	return running, completed, nil
}
func (a *App) execute(call Item) error {
	output := ""
	switch str(call["name"]) {
	case "plan":
		var p struct {
			Text *string `json:"text"`
		}
		if e := decodeArgs(call, &p); e != nil || p.Text == nil {
			output = "ERROR: plan requires text"
		} else {
			if e = a.T.Append(Item{"type": "ply.plan", "text": *p.Text}); e != nil {
				return e
			}
			output = "Plan updated."
		}
	case "bash":
		var args BashArgs
		if e := decodeArgs(call, &args); e != nil || args.Command == "" || args.Timeout < 0 {
			output = "ERROR: invalid bash arguments"
			break
		}
		if args.Timeout == 0 {
			args.Timeout = a.Config.N("bash.default_timeout")
		}
		ok, reason, e := a.approve(call, args, 0)
		if e != nil {
			return e
		}
		if !ok {
			output = "DENIED: " + reason
			break
		}
		if args.Background {
			output, e = a.startTask(call, args)
			if e != nil {
				return e
			}
			break
		}
		dir := filepath.Join(a.Config.Project, "out")
		if e = os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		path := filepath.Join(dir, fmt.Sprintf("%x-%s-%d", sha256.Sum256([]byte(a.Path)), safeName(str(call["call_id"])), num(call["seq"])))
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		code, duration, timed, runErr := a.foreground(args, f)
		f.Close()
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		output = truncate(string(b), a.Config.N("output.max_lines"), path, false)
		if runErr != nil {
			output += "\nERROR: " + runErr.Error()
		}
		output += fmt.Sprintf("\n[exit %d, %.2fs]", code, duration)
		if timed {
			output += "\nTIMED_OUT"
		}
		if a.Context.Err() != nil || len(a.Steers) > 0 {
			output += "\nINTERRUPTED"
		}
	default:
		output = "ERROR: unknown tool " + str(call["name"])
	}
	if e := a.T.Append(Item{"type": "function_call_output", "call_id": call["call_id"], "output": output}); e != nil {
		return e
	}
	if a.Context.Err() != nil || len(a.Steers) > 0 {
		a.T.Append(Item{"type": "ply.interrupt", "during": "command", "for": call["seq"]})
		if a.Context.Err() != nil {
			return a.Context.Err()
		}
		return errSteered
	}
	return nil
}

// Keep the bridge live while a foreground shell blocks on a child task.
func (a *App) foreground(args BashArgs, out io.Writer) (int, float64, bool, error) {
	ctx, cancel := context.WithCancel(a.Context)
	defer cancel()
	type result struct {
		code     int
		duration float64
		timed    bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		code, duration, timed, e := runProcess(ctx, a.Config.S("bash.shell"), args.Command, a.Cwd, out, nil, args.Timeout)
		done <- result{code, duration, timed, e}
	}()
	var commands <-chan Item
	if a.Protocol != nil {
		commands = a.Protocol.Commands
	}
	for {
		select {
		case r := <-done:
			return r.code, r.duration, r.timed, r.err
		case p := <-a.Proxy:
			if e := a.proxy(p); e != nil {
				cancel()
				r := <-done
				return r.code, r.duration, r.timed, e
			}
		case i, open := <-commands:
			if !open {
				commands = nil
				continue
			}
			if str(i["cmd"]) == "steer" {
				a.Steers = append(a.Steers, str(i["message"]))
				cancel()
			}
		}
	}
}
