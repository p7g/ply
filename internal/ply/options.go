package ply

import (
	"fmt"
	"strconv"
	"strings"
)

type Options struct {
	Path, File, Config, Provider, PlanFrom, Kill                                                                                         string
	Messages                                                                                                                             []string
	Overrides                                                                                                                            map[string]string
	Edit, AllowEmpty, Plan, Subagent, NoTools, Compact, Clear, ShowPlan, Tasks, ShowConfig, Refresh, Trust, Follow, Quiet, Help, Version bool
	History, Tail                                                                                                                        int
	HasFile, HasKill                                                                                                                     bool
}

func Parse(args []string) (Options, error) {
	o := Options{Overrides: map[string]string{}, Tail: -1}
	positionals := []string{}
	killPosition := -1
	keys := map[string]string{}
	for k := range defaults {
		keys[strings.NewReplacer(".", "-", "_", "-").Replace(k)] = k
	}
	keys["show-thinking"] = "output.show_thinking"
	for n := 0; n < len(args); n++ {
		a := args[n]
		if a == "--" {
			for _, p := range args[n+1:] {
				positionals = append(positionals, p)
			}
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positionals = append(positionals, a)
			continue
		}
		name, val, has := strings.Cut(a, "=")
		value := func() (string, error) {
			if has {
				return val, nil
			}
			n++
			if n >= len(args) {
				return "", fmt.Errorf("%s requires a value", name)
			}
			return args[n], nil
		}
		boolval := true
		inverse := strings.HasPrefix(name, "--no-") && name != "--no-tools"
		if strings.HasPrefix(name, "--no-") && name != "--no-tools" {
			name = "--" + strings.TrimPrefix(name, "--no-")
			boolval = false
		}
		if has {
			if b, e := strconv.ParseBool(val); e == nil {
				boolval = b
				if inverse {
					boolval = !b
				}
			}
		}
		switch name {
		case "--help", "-h":
			o.Help = true
		case "--version":
			o.Version = true
		case "-m":
			s, e := value()
			if e != nil {
				return o, e
			}
			o.Messages = append(o.Messages, s)
		case "-F":
			s, e := value()
			if e != nil {
				return o, e
			}
			o.File = s
			o.HasFile = true
		case "--config":
			s, e := value()
			if e != nil {
				return o, e
			}
			o.Config = s
		case "--provider":
			s, e := value()
			if e != nil {
				return o, e
			}
			o.Provider = s
		case "--plan-from":
			s, e := value()
			if e != nil {
				return o, e
			}
			o.PlanFrom = s
		case "--kill":
			o.HasKill = true
			o.Kill = "all"
			if has {
				o.Kill = val
			} else {
				killPosition = len(positionals)
			}
		case "--tail":
			s, e := value()
			if e != nil {
				return o, e
			}
			o.Tail, e = strconv.Atoi(s)
			if e != nil || o.Tail < 0 {
				return o, fmt.Errorf("--tail requires a nonnegative integer")
			}
		case "-H":
			o.History = 1
		case "-HH":
			o.History = 2
		case "-e":
			o.Edit = boolval
		case "--allow-empty":
			o.AllowEmpty = boolval
		case "--plan":
			o.Plan = boolval
		case "--subagent":
			o.Subagent = boolval
		case "--no-tools":
			o.NoTools = boolval
		case "--tools":
			o.NoTools = false
		case "--compact":
			o.Compact = boolval
		case "--clear":
			o.Clear = boolval
		case "--show-plan":
			o.ShowPlan = boolval
		case "--tasks":
			o.Tasks = boolval
		case "--show-config":
			o.ShowConfig = boolval
		case "--refresh-system":
			o.Refresh = boolval
		case "--trust-project":
			o.Trust = boolval
		case "-f", "--follow":
			o.Follow = boolval
		case "-q", "--quiet":
			o.Quiet = boolval
		default:
			k, ok := keys[strings.TrimPrefix(name, "--")]
			if !ok {
				return o, fmt.Errorf("unknown option %s", a)
			}
			if _, ok := defaults[k].(bool); ok {
				if has {
					if _, e := strconv.ParseBool(val); e != nil {
						return o, e
					}
				}
				o.Overrides[k] = strconv.FormatBool(boolval)
			} else {
				if inverse {
					return o, fmt.Errorf("%s is not boolean", name)
				}
				s, e := value()
				if e != nil {
					return o, e
				}
				o.Overrides[k] = s
			}
		}
	}
	if len(positionals) == 2 && o.HasKill && killPosition >= 0 {
		if killPosition == 0 {
			o.Kill = positionals[0]
			o.Path = positionals[1]
		} else {
			o.Path = positionals[0]
			o.Kill = positionals[1]
		}
	} else if len(positionals) == 1 {
		o.Path = positionals[0]
	} else if len(positionals) > 1 {
		return o, fmt.Errorf("only one transcript allowed")
	}
	if !o.Help && !o.Version && o.Path == "" {
		return o, fmt.Errorf("usage: ply [options] TRANSCRIPT")
	}
	actions := 0
	for _, b := range []bool{o.Compact, o.Clear, o.ShowPlan, o.Tasks, o.HasKill, o.ShowConfig} {
		if b {
			actions++
		}
	}
	if actions > 1 {
		return o, fmt.Errorf("choose one transcript action")
	}
	if o.Follow && (len(o.Messages) > 0 || o.HasFile || o.Edit || o.AllowEmpty || actions > 0 || o.PlanFrom != "" || o.Subagent || o.Refresh) {
		return o, fmt.Errorf("follow is incompatible with a turn or action")
	}
	if o.Subagent && (actions > 0 || o.Edit) {
		return o, fmt.Errorf("subagent mode requires a turn and reserves stdin for JSONL")
	}
	if actions > 0 && (len(o.Messages) > 0 || o.HasFile || o.Edit || o.AllowEmpty) {
		return o, fmt.Errorf("action does not accept a user message")
	}
	return o, nil
}

const Help = `ply [options] TRANSCRIPT

  -m TEXT              Run a turn; repeat for multiple paragraphs
  -F FILE              Read message from FILE (- for stdin)
  -e                   Edit message with $EDITOR
  --allow-empty        Run without a user message
  --plan / --no-tools / --subagent
  --compact / --clear / --plan-from FILE / --show-plan
  --tasks / --kill[=ID] / --show-config
  -H / -HH             Render context / full history before turn
  -f, --follow         Follow transcript (read-only)
  --tail N / --show-thinking / -q / --no-pager
  --model NAME --context-window N --base-url URL
  --provider replay:FILE
  --config FILE / --trust-project / --refresh-system

All configuration keys have long flags (e.g. --output-max-lines).
Boolean flags accept --no- inverses. See README.md for configuration.
`
