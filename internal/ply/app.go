package ply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
)

const Version = "0.1.0"

type App struct {
	T          *Transcript
	Options    Options
	Config     Config
	Path, Cwd  string
	Context    context.Context
	Provider   *Provider
	Renderer   Renderer
	Proxy      chan proxyRequest
	Protocol   *Protocol
	Children   []io.WriteCloser
	Steers     []string
	ApprovalID int
	Streamed   bool
	Status     *statusLine
}

func Main(args []string) int {
	if len(args) == 2 && args[0] == "--internal-worker" {
		return Worker(args[1])
	}
	o, e := Parse(args)
	if e != nil {
		fmt.Fprintln(os.Stderr, "ply:", e)
		return 2
	}
	if o.Help {
		fmt.Print(Help)
		return 0
	}
	if o.Version {
		fmt.Println(Version)
		return 0
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	e = run(ctx, o)
	if e != nil {
		if o.Subagent {
			event(Item{"event": "error", "message": e.Error()})
		}
		fmt.Fprintln(os.Stderr, "ply:", e)
		if errors.Is(e, context.Canceled) {
			return 130
		}
		return 1
	}
	return 0
}
func run(ctx context.Context, o Options) error {
	cwd, e := os.Getwd()
	if e != nil {
		return e
	}
	path, e := filepath.Abs(o.Path)
	if e != nil {
		return e
	}
	items, e := readItems(path, o.Follow)
	if e != nil {
		return e
	}
	if len(items) > 0 {
		cwd = str(items[0]["cwd"])
		if !filepath.IsAbs(cwd) {
			return fmt.Errorf("transcript cwd must be absolute")
		}
	}
	c, e := resolve(cwd, o)
	if e != nil {
		return e
	}
	color := c.S("output.color") == "always" || c.S("output.color") == "auto" && tty(os.Stdout)
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		color = false
	}
	status := &statusLine{W: os.Stderr, Enabled: tty(os.Stderr) && !o.Subagent}
	defer status.Clear()
	r := Renderer{BeforeWrite: status.Clear, W: os.Stdout, Quiet: o.Quiet, Thinking: c.B("output.show_thinking"), Color: color}
	action := o.Compact || o.Clear || o.ShowPlan || o.Tasks || o.HasKill || o.ShowConfig
	msg, turn, e := inputMessage(o, items, action)
	if e != nil {
		return e
	}
	if o.Edit && !turn {
		return nil
	}
	if o.Follow {
		turn = false
	}
	if o.Subagent {
		turn = true
	}
	if o.ShowConfig {
		keys := []string{}
		for k := range c.Values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%-24s %-40v %s\n", k, c.Values[k], c.Sources[k])
		}
		return nil
	}
	if o.ShowPlan {
		if i := latest(items, "ply.plan"); i != nil {
			page(str(i["text"])+"\n", c)
		}
		return nil
	}
	if !turn && !action && o.PlanFrom == "" && !o.Refresh {
		if _, e = os.Stat(path); os.IsNotExist(e) {
			t, e := openTranscript(path, cwd)
			if e != nil {
				return e
			}
			items = t.Items
			t.Close()
		}
		renderItems(items, o, r, c)
		if o.Follow {
			seen := len(items)
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(200 * time.Millisecond):
					newItems, e := readItems(path, true)
					if e != nil {
						return e
					}
					if len(newItems) < seen {
						return fmt.Errorf("transcript was truncated while following")
					}
					for _, i := range newItems[seen:] {
						r.Item(i)
					}
					seen = len(newItems)
				}
			}
		}
		return nil
	}
	if turn || o.Compact {
		if e = c.require(); e != nil {
			return e
		}
	}
	t, e := openTranscript(path, cwd)
	if e != nil {
		return e
	}
	defer t.Close()
	a := &App{T: t, Options: o, Config: c, Path: path, Cwd: cwd, Context: ctx, Renderer: r, Status: status, Proxy: make(chan proxyRequest, 32)}
	defer func() {
		for _, p := range a.Children {
			p.Close()
		}
	}()
	if o.Subagent {
		a.Protocol = newProtocol()
		event(Item{"event": "hello", "pid": os.Getpid(), "transcript": path})
	}
	t.OnAppend = func(i Item) {
		if !o.Subagent {
			r.Item(i)
		}
	}
	if o.History > 0 && !o.Subagent {
		renderItems(t.Items, o, r, c)
	}
	fail := func(err error) error {
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Append(Item{"type": "ply.error", "kind": "harness", "message": err.Error(), "retries": c.N("provider.retries")})
		}
		return err
	}
	last := latest(t.Items, "ply.config")
	b, _ := json.Marshal(c.Values)
	old, _ := json.Marshal(last["values"])
	if !reflect.DeepEqual(b, old) {
		if e = t.Append(Item{"type": "ply.config", "values": c.Values}); e != nil {
			return e
		}
	}
	if o.PlanFrom != "" {
		other, e := readItems(o.PlanFrom, false)
		if e != nil {
			return fail(e)
		}
		p := latest(other, "ply.plan")
		if p == nil {
			return fail(fmt.Errorf("no plan in %s", o.PlanFrom))
		}
		if e = t.Append(Item{"type": "ply.plan", "text": p["text"]}); e != nil {
			return e
		}
	}
	if o.Tasks || o.HasKill {
		if _, _, e = a.scanTasks(); e != nil {
			return fail(e)
		}
		if o.HasKill {
			found := false
			done := map[string]bool{}
			for _, i := range t.Items {
				if str(i["type"]) == "ply.task_done" {
					done[str(i["task"])] = true
				}
			}
			for _, i := range t.Items {
				if str(i["type"]) == "ply.task_started" && !done[str(i["task"])] && (o.Kill == "all" || o.Kill == str(i["task"])) {
					found = true
					if e = syscall.Kill(num(i["pid"]), syscall.SIGTERM); e != nil && e != syscall.ESRCH {
						return fail(e)
					}
				}
			}
			if !found && o.Kill != "all" {
				return fail(fmt.Errorf("no running task %s", o.Kill))
			}
		}
		_, _, e = a.scanTasks()
		if e != nil {
			return fail(e)
		}
		if o.Tasks {
			done := map[string]bool{}
			for _, i := range t.Items {
				if str(i["type"]) == "ply.task_done" {
					done[str(i["task"])] = true
				}
			}
			for _, i := range t.Items {
				if str(i["type"]) == "ply.task_started" {
					status := "running"
					if done[str(i["task"])] {
						status = "done"
					}
					fmt.Printf("%s\t%s\t%s\n", str(i["task"]), status, str(i["command"]))
				}
			}
		}
		return nil
	}
	if o.Clear {
		if len(pending(t.Items)) > 0 {
			return fail(fmt.Errorf("cannot clear with unresolved tool calls; run --allow-empty to recover"))
		}
		return t.Append(Item{"type": "ply.compaction", "summary": nil, "plan": str(latest(t.Items, "ply.plan")["text"]), "tokens_before": num(latest(t.Items, "ply.usage")["input_tokens"]), "tokens_after": 0})
	}
	if turn || o.Compact || o.Refresh {
		if latest(t.Items, "ply.system") == nil || o.Refresh {
			s, sources, e := systemPrompt(ctx, cwd, c)
			if e != nil {
				return fail(e)
			}
			if e = t.Append(Item{"type": "ply.system", "text": s, "sources": sources}); e != nil {
				return e
			}
		}
	}
	if !turn && !o.Compact {
		return nil
	}
	a.Provider, e = newProvider(c, o.Provider)
	if e != nil {
		return fail(e)
	}
	if o.Compact {
		return fail(a.compact())
	}
	if _, _, e = a.scanTasks(); e != nil {
		return fail(e)
	}
	for _, mode := range []struct {
		active     bool
		name, text string
	}{{o.Plan, "plan", "For this turn: produce or revise the plan using the plan tool. Do not execute commands other than read-only inspection needed to plan."}, {o.Subagent, "subagent", "For this turn: work as a subagent, report a concise final result, and wait for your background tasks."}, {o.NoTools, "no_tools", "For this turn: answer without tools in a single response."}} {
		if mode.active {
			i := message("developer", mode.text)
			i["ply.mode"] = mode.name
			if e = t.Append(i); e != nil {
				return e
			}
		}
	}
	if msg != "" {
		if e = t.Append(message("user", msg)); e != nil {
			return e
		}
	} // Never rerun an ambiguous call after a crash: it may already have had effects.
	for _, call := range pending(t.Items) {
		if e = t.Append(Item{"type": "function_call_output", "call_id": call["call_id"], "output": "INTERRUPTED: previous invocation ended before recording this result; inspect state before retrying."}); e != nil {
			return e
		}
	}
	result := ""
	for {
		for _, s := range a.Steers {
			if e = t.Append(message("user", s)); e != nil {
				return e
			}
		}
		a.Steers = nil
		if _, _, e = a.scanTasks(); e != nil {
			return fail(e)
		}
		if a.needsCompaction() {
			if e = a.compact(); e != nil {
				return fail(e)
			}
		}
		resp, e := a.call(Replay(t.Items), !o.NoTools, true)
		if errors.Is(e, errSteered) {
			continue
		}
		if e != nil {
			return fail(e)
		}
		calls := []Item{}
		result = ""
		for _, i := range resp.Output {
			if str(i["type"]) == "message" && str(i["role"]) == "assistant" {
				result += textOf(i)
				if a.Streamed {
					e = a.appendSilent(i)
				} else {
					e = t.Append(i)
				}
			} else {
				e = t.Append(i)
			}
			if e != nil {
				return e
			}
			if str(i["type"]) == "function_call" {
				calls = append(calls, i)
			}
		}
		if e = a.usage(resp); e != nil {
			return e
		}
		if o.NoTools {
			break
		}
		if len(calls) > 0 {
			for _, call := range calls {
				if e = a.execute(call); e != nil {
					if errors.Is(e, errSteered) {
						for _, p := range pending(t.Items) {
							if e = t.Append(Item{"type": "function_call_output", "call_id": p["call_id"], "output": "INTERRUPTED: steered before execution"}); e != nil {
								return e
							}
						}
						break
					}
					return fail(e)
				}
				if _, _, e = a.scanTasks(); e != nil {
					return fail(e)
				}
			}
			continue
		}
		running, completed, e := a.scanTasks()
		if e != nil {
			return fail(e)
		}
		if completed > 0 {
			continue
		}
		if running == 0 {
			break
		}
		if c.B("detach") && !o.Subagent {
			if !o.Quiet {
				fmt.Printf("[detached: %d tasks running; subagents will be denied approvals]\n", running)
			}
			break
		}
		a.Status.Show(fmt.Sprintf("[waiting: %d tasks]", running))
		var commands <-chan Item
		if a.Protocol != nil {
			commands = a.Protocol.Commands
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				a.Status.Clear()
				if !o.Quiet && !o.Subagent {
					fmt.Printf("[detached: %d tasks running]\n", running)
				}
				return nil
			case p := <-a.Proxy:
				a.Status.Clear()
				if e = a.proxy(p); e != nil {
					return fail(e)
				}
			case cmd, open := <-commands:
				if !open {
					commands = nil
					continue
				}
				if str(cmd["cmd"]) == "steer" {
					a.Steers = append(a.Steers, str(cmd["message"]))
					break wait
				}
			case <-time.After(100 * time.Millisecond):
				var finished int
				running, finished, e = a.scanTasks()
				if e != nil {
					return fail(e)
				}
				if finished > 0 || running == 0 {
					break wait
				}
			}
		}
	}
	a.Status.Clear()
	if o.Plan && !o.Subagent {
		if p := latest(t.Items, "ply.plan"); p != nil {
			fmt.Println(str(p["text"]))
		}
	}
	if o.Subagent {
		event(Item{"event": "result", "text": result})
	}
	return nil
}
func (a *App) appendSilent(i Item) error {
	f := a.T.OnAppend
	a.T.OnAppend = nil
	e := a.T.Append(i)
	a.T.OnAppend = f
	return e
}
func (a *App) usage(r Response) error {
	last := latest(a.T.Items, "ply.usage")
	return a.T.Append(Item{"type": "ply.usage", "model": a.Config.S("model"), "input_tokens": r.Usage.Input, "output_tokens": r.Usage.Output, "cached_tokens": r.Usage.Details.Cached, "total_input": num(last["total_input"]) + r.Usage.Input, "total_output": num(last["total_output"]) + r.Usage.Output})
}
func (a *App) needsCompaction() bool {
	for n := len(a.T.Items) - 1; n >= 0; n-- {
		i := a.T.Items[n]
		if str(i["type"]) == "ply.compaction" {
			return false
		}
		if str(i["type"]) == "ply.usage" {
			return num(i["input_tokens"]) > a.Config.Threshold()
		}
	}
	return false
}
func (a *App) compact() error {
	if len(pending(a.T.Items)) > 0 {
		return fmt.Errorf("cannot compact with unresolved tool calls")
	}
	before := num(latest(a.T.Items, "ply.usage")["input_tokens"])
	input := Replay(a.T.Items)
	input = append(input, message("user", "Summarize this context for continuation. Preserve the outstanding user request, in-progress work, task IDs, important paths, decisions, and unresolved problems. The plan is preserved separately verbatim. Return only the summary."))
	r, e := a.call(input, false, false)
	if e != nil {
		return e
	}
	summary := ""
	for _, i := range r.Output {
		if str(i["type"]) == "message" {
			summary += textOf(i)
		}
	}
	if strings.TrimSpace(summary) == "" {
		return fmt.Errorf("compaction returned an empty summary")
	}
	if e = a.usage(r); e != nil {
		return e
	}
	return a.T.Append(Item{"type": "ply.compaction", "summary": summary, "plan": str(latest(a.T.Items, "ply.plan")["text"]), "tokens_before": before, "tokens_after": r.Usage.Output})
}
func renderItems(items []Item, o Options, r Renderer, c Config) {
	var b strings.Builder
	r.W = &b
	for _, i := range visible(items, o.History == 2, o.Tail) {
		r.Item(i)
	}
	if o.Follow {
		fmt.Print(b.String())
	} else {
		page(b.String(), c)
	}
}
func inputMessage(o Options, items []Item, action bool) (string, bool, error) {
	parts := append([]string{}, o.Messages...)
	turn := len(parts) > 0 || o.HasFile || o.Edit || o.AllowEmpty
	if o.HasFile {
		var b []byte
		var e error
		if o.File == "-" {
			if o.Subagent {
				return "", false, fmt.Errorf("subagent stdin is reserved for protocol commands")
			}
			b, e = io.ReadAll(os.Stdin)
		} else {
			b, e = os.ReadFile(o.File)
		}
		if e != nil {
			return "", false, e
		}
		parts = append(parts, string(b))
	} else if !turn && !action && !o.Follow && !o.Subagent && !tty(os.Stdin) {
		b, e := io.ReadAll(os.Stdin)
		if e != nil {
			return "", false, e
		}
		if len(b) > 0 {
			parts = append(parts, string(b))
			turn = true
		}
	}
	s := strings.Join(parts, "\n\n")
	if o.Edit {
		f, e := os.CreateTemp("", "ply-message-*.txt")
		if e != nil {
			return "", false, e
		}
		defer os.Remove(f.Name())
		var tail strings.Builder
		r := Renderer{W: &tail}
		for _, i := range visible(items, false, 12) {
			r.Item(i)
		}
		_, e = f.WriteString(s + "\n\n" + indent(tail.String(), "# ") + "\n")
		f.Close()
		if e != nil {
			return "", false, e
		}
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		cmd := exec.Command("/bin/sh", "-c", editor+" \"$1\"", "ply-editor", f.Name())
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if e = cmd.Run(); e != nil {
			return "", false, e
		}
		b, e := os.ReadFile(f.Name())
		if e != nil {
			return "", false, e
		}
		var lines []string
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(l, "#") {
				lines = append(lines, l)
			}
		}
		s = strings.TrimSpace(strings.Join(lines, "\n"))
		if s == "" {
			return "", false, nil
		}
	}
	if turn && strings.TrimSpace(s) == "" && !o.AllowEmpty {
		return "", false, fmt.Errorf("empty message; use --allow-empty")
	}
	return s, turn, nil
}
