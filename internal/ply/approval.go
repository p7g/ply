package ply

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type proxyRequest struct {
	Event Item
	Input io.Writer
	For   any
}
type Protocol struct {
	Commands chan Item
}

func newProtocol() *Protocol {
	p := &Protocol{Commands: make(chan Item, 32)}
	go func() {
		defer close(p.Commands)
		input := os.Stdin
		// Keep bash stdin at /dev/null; nested ply receives the protocol on fd 3.
		if os.Getenv("PLY_PARENT_INPUT_FD") == "3" {
			if f := os.NewFile(3, "ply-parent"); f != nil {
				input = f
			}
		}
		r := bufio.NewReader(input)
		for {
			line, e := r.ReadBytes('\n')
			if len(line) > 0 {
				var i Item
				if json.Unmarshal(line, &i) == nil {
					p.Commands <- i
				}
			}
			if e != nil {
				return
			}
		}
	}()
	return p
}
func event(i Item) { b, _ := json.Marshal(i); fmt.Println(string(b)) }
func (a *App) approve(call Item, args BashArgs, depth int) (bool, string, error) {
	a.Status.Clear()
	var ok bool
	var reason string
	approver := a.Config.S("approve.command")
	if a.Options.Subagent {
		approver = "parent bridge"
		id := fmt.Sprintf("a%d", a.ApprovalID)
		a.ApprovalID++
		event(Item{"event": "approval_request", "id": id, "command": args.Command, "justification": args.Justification, "background": args.Background, "timeout_s": args.Timeout, "depth": depth, "plan_mode": a.Options.Plan})
	waiting:
		for {
			select {
			case i, open := <-a.Protocol.Commands:
				if !open {
					reason = "no parent attached"
					break waiting
				}
				switch str(i["cmd"]) {
				case "approve":
					if str(i["id"]) == id {
						ok, _ = i["ok"].(bool)
						reason = str(i["reason"])
						break waiting
					}
				case "steer":
					a.Steers = append(a.Steers, str(i["message"]))
					reason = "interrupted by steering"
					break waiting
				}
			case <-a.Context.Done():
				return false, "interrupted", a.Context.Err()
			}
		}
	} else {
		command := args.Command
		truncated := "0"
		const maxCommand = 128*1024 - len("PLY_COMMAND=") - 1
		if len(command) > maxCommand {
			command = command[:maxCommand]
			truncated = "1"
		}
		user := ""
		for n := len(a.T.Items) - 1; n >= 0; n-- {
			i := a.T.Items[n]
			if str(i["type"]) == "message" && str(i["role"]) == "user" {
				user = textOf(i)
				break
			}
		}
		env := map[string]string{"PLY_COMMAND": command,
			"PLY_COMMAND_TRUNCATED":      truncated,
			"PLY_JUSTIFICATION":          args.Justification,
			"PLY_USER_MSG":               user,
			"PLY_CWD":                    a.Cwd,
			"PLY_TRANSCRIPT":             a.Path,
			"PLY_BACKGROUND":             strconv.Itoa(btoi(args.Background)),
			"PLY_TIMEOUT":                strconv.Itoa(args.Timeout),
			"PLY_APPROVAL_DEPTH":         strconv.Itoa(depth),
			"PLY_CONFIG_DIR":             a.Config.Project,
			"PLY_PLAN_MODE":              strconv.Itoa(btoi(a.Options.Plan)),
			"PLY_API_KEY":                a.Config.S("api_key"),
			"PLY_BASE_URL":               a.Config.S("base_url"),
			"PLY_MODEL":                  a.Config.S("model"),
			"PLY_CONTEXT_WINDOW":         strconv.Itoa(a.Config.N("context_window")),
			"PLY_APPROVE_MODEL":          a.Config.S("approve.model"),
			"PLY_APPROVE_CONTEXT_WINDOW": strconv.Itoa(a.Config.N("approve.context_window")),
			"PLY_PROVIDER_RETRIES":       strconv.Itoa(a.Config.N("provider.retries"))}
		cmd := exec.CommandContext(a.Context, "/bin/sh", "-c", approver)
		cmd.Dir = a.Cwd
		// Interactive approvers must remain in the terminal's foreground group.
		// A separate group receives SIGTTIN when it reads /dev/tty.
		cmd.WaitDelay = time.Second
		env["PLY_COMMAND_RENDERED"] = strconv.Itoa(btoi(depth == 0 && a.CommandRendered))
		cmd.Env = replaceEnv(os.Environ(), env)
		cmd.Stderr = os.Stderr
		b, e := cmd.Output()
		if a.Context.Err() != nil {
			if e := a.T.Append(Item{"type": "ply.interrupt", "during": "approval", "for": call["seq"]}); e != nil {
				return false, "", e
			}
			return false, "interrupted", a.Context.Err()
		}
		ok = e == nil
		reason = strings.TrimSpace(string(b))
		if e != nil && reason == "" {
			reason = e.Error()
		}
		if !ok && reason == "" {
			reason = "approver denied or had no opinion"
		}
	}
	result := "denied"
	if ok {
		result = "approved"
	}
	e := a.T.Append(Item{"type": "ply.approval", "for": call["seq"], "command": args.Command, "result": result, "reason": reason, "approver": approver, "depth": depth})
	return ok, reason, e
}
func replaceEnv(base []string, values map[string]string) []string {
	out := []string{}
	for _, s := range base {
		k, _, _ := strings.Cut(s, "=")
		if _, ok := values[k]; !ok {
			out = append(out, s)
		}
	}
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	return out
}
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
func (a *App) proxy(p proxyRequest) error {
	args := BashArgs{Command: str(p.Event["command"]), Justification: str(p.Event["justification"]), Timeout: num(p.Event["timeout_s"])}
	args.Background, _ = p.Event["background"].(bool)
	originPlan := a.Options.Plan
	a.Options.Plan = originPlan || p.Event["plan_mode"] == true
	ok, reason, e := a.approve(Item{"seq": p.For}, args, max(0, num(p.Event["depth"]))+1)
	a.Options.Plan = originPlan
	if e != nil {
		return e
	}
	b, _ := json.Marshal(Item{"cmd": "approve", "id": p.Event["id"], "ok": ok, "reason": reason})
	_, e = p.Input.Write(append(b, '\n'))
	if e != nil {
		return nil
	}
	return nil
}
func (a *App) call(input []Item, tools bool, stream bool) (Response, error) {
	ctx, cancel := context.WithCancel(a.Context)
	defer cancel()
	type result struct {
		r Response
		s string
		e error
	}
	ch := make(chan result, 1)
	type chunk struct {
		text      string
		reasoning bool
		index     int
	}
	deltas := make(chan chunk, 64)
	a.ReasoningStreamed = map[int]bool{}
	a.Provider.SummaryDelta = func(index int, s string) {
		select {
		case deltas <- chunk{text: s, reasoning: true, index: index}:
		case <-ctx.Done():
		}
	}
	defer func() { a.Provider.SummaryDelta = nil }()
	go func() {
		r, s, e := a.Provider.Call(ctx, input, tools, func(s string) {
			select {
			case deltas <- chunk{text: s}:
			case <-ctx.Done():
			}
		})
		ch <- result{r, s, e}
	}()
	label := "thinking..."
	if !stream {
		label = "compacting..."
	}
	a.Status.Show(label)
	defer a.Status.Clear()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	prose := proseStream{W: os.Stdout, BeforeWrite: a.Status.Clear}
	summary := proseStream{W: os.Stdout, BeforeWrite: a.Status.Clear}
	printDelta := func(c chunk) {
		if c.reasoning {
			if stream && !a.Options.Subagent && !a.Options.Quiet && a.Config.B("output.show_thinking") {
				if !summary.Started && strings.TrimSpace(c.text) == "" {
					return
				}
				if !summary.Started {
					a.Status.Clear()
					fmt.Fprint(os.Stdout, "~ ")
				}
				summary.Delta(c.text)
				if summary.Started {
					a.ReasoningStreamed[c.index] = true
				}
			}
			return
		}
		summary.End()
		summary.Started = false
		s := c.text
		if stream && !a.Options.Subagent {
			prose.Delta(s)
		}
	}
	var commands <-chan Item
	if a.Protocol != nil {
		commands = a.Protocol.Commands
	}
	for {
		select {
		case <-ticker.C:
			if !prose.Started {
				a.Status.Tick()
			}
		case s := <-deltas:
			printDelta(s)
		case p := <-a.Proxy:
			a.Status.Clear()
			if e := a.proxy(p); e != nil {
				cancel()
				<-ch
				return Response{}, e
			}
			if !prose.Started {
				a.Status.Show(label)
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
		case res := <-ch:
			for len(deltas) > 0 {
				printDelta(<-deltas)
			}
			a.Status.Clear()
			summary.End()
			prose.End()
			a.Streamed = prose.Started
			if res.e != nil && ctx.Err() != nil {
				if res.s != "" && stream {
					i := message("assistant", res.s)
					i["ply.partial"] = true
					if e := a.appendSilent(i); e != nil {
						return Response{}, e
					}
				}
				if e := a.T.Append(Item{"type": "ply.interrupt", "during": "model", "for": nil}); e != nil {
					return Response{}, e
				}
				if len(a.Steers) > 0 && a.Context.Err() == nil {
					return Response{}, errSteered
				}
			}
			return res.r, res.e
		}
	}
}

var errSteered = fmt.Errorf("steered")
