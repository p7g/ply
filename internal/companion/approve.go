package companion

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"ply/internal/prompts"
	"regexp"
	"strings"
)

func UserDir() string {
	if s := os.Getenv("XDG_CONFIG_HOME"); s != "" {
		return filepath.Join(s, "ply")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "ply")
}
func Approve(kind string, args []string) int {
	if len(args) > 0 && args[0] == "--ply-prompt" {
		return 0
	}
	switch kind {
	case "yolo":
		return 0
	case "ask":
		f, e := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if e != nil {
			fmt.Println("no terminal available")
			return 1
		}
		defer f.Close()
		if os.Getenv("PLY_COMMAND_RENDERED") != "1" {
			fmt.Fprintf(f, "$ %s\n", os.Getenv("PLY_COMMAND"))
			if why := os.Getenv("PLY_JUSTIFICATION"); why != "" {
				fmt.Fprintln(f, why)
			}
		}
		if depth := os.Getenv("PLY_APPROVAL_DEPTH"); depth != "" && depth != "0" {
			fmt.Fprint(f, "[subagent] ")
		}
		fmt.Fprint(f, "Allow? [y/N] ")
		line, _ := bufio.NewReader(f).ReadString('\n')
		if strings.EqualFold(strings.TrimSpace(line), "y") {
			return 0
		}
		fmt.Println("declined by user")
		return 1
	case "allowlist":
		path := filepath.Join(os.Getenv("PLY_CONFIG_DIR"), "allowlist")
		b, e := os.ReadFile(path)
		if os.IsNotExist(e) || os.Getenv("PLY_CONFIG_DIR") == "" {
			b, e = os.ReadFile(filepath.Join(UserDir(), "allowlist"))
		}
		if os.IsNotExist(e) {
			return 2
		}
		if e != nil {
			fmt.Println(e)
			return 1
		}
		for n, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			verb, pattern, ok := strings.Cut(line, " ")
			if !ok || (verb != "allow" && verb != "deny") {
				fmt.Printf("invalid allowlist rule on line %d\n", n+1)
				return 1
			}
			r, e := regexp.Compile(strings.TrimSpace(pattern))
			if e != nil {
				fmt.Println(e)
				return 1
			}
			if r.MatchString(os.Getenv("PLY_COMMAND")) {
				if verb == "allow" {
					return 0
				}
				fmt.Printf("denied by allowlist rule %d\n", n+1)
				return 1
			}
		}
		return 2
	case "chain":
		var reasons []string
		for _, program := range args {
			cmd := exec.Command(program)
			cmd.Stdin = os.Stdin
			cmd.Stderr = os.Stderr
			b, e := cmd.Output()
			code := exitCode(e)
			if code == 2 {
				if reason := strings.TrimSpace(string(b)); reason != "" {
					reasons = append(reasons, reason)
				}
				continue
			}
			os.Stdout.Write(b)
			return code
		}
		if len(reasons) > 0 {
			fmt.Println(strings.Join(reasons, "\n"))
		}
		return 2
	case "auto":
		dir, e := os.MkdirTemp("", "ply-approve-*")
		if e != nil {
			fmt.Println(e)
			return 1
		}
		defer os.RemoveAll(dir)

		prompt, e := approvalPrompt()
		if e != nil {
			fmt.Println("could not render approval prompt:", e)
			return 2
		}
		cmd := exec.Command("ply", "--no-tools", "-q", "-m", prompt, filepath.Join(dir, "approval.jsonl"))
		cmd.Env = approvalEnvironment()
		cmd.Stderr = os.Stderr
		b, e := cmd.Output()
		if e != nil {
			fmt.Println("model approver failed:", e)
			return 2
		}
		decision, reason, e := parseDecision(string(b))
		if e != nil {
			fmt.Println("invalid model approval response:", e)
			return 2
		}
		switch decision {
		case "APPROVE":
			return 0
		case "DENY":
			fmt.Println(reason)
			return 1
		default:
			fmt.Println(reason)
			return 2
		}

	}
	return 2
}
func exitCode(e error) int {
	if e == nil {
		return 0
	}
	if ee, ok := e.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	fmt.Fprintln(os.Stderr, e)
	return 1
}

func approvalPrompt() (string, error) {
	return prompts.RenderApproval(prompts.ApprovalContext{
		Command:          os.Getenv("PLY_COMMAND"),
		UserRequest:      os.Getenv("PLY_USER_MSG"),
		Justification:    os.Getenv("PLY_JUSTIFICATION"),
		WorkingDirectory: os.Getenv("PLY_CWD"),
		TranscriptPath:   os.Getenv("PLY_TRANSCRIPT"),
		TimeoutSeconds:   os.Getenv("PLY_TIMEOUT"),
		SubagentDepth:    os.Getenv("PLY_APPROVAL_DEPTH"),
		PlanMode:         os.Getenv("PLY_PLAN_MODE") == "1",
		Background:       os.Getenv("PLY_BACKGROUND") == "1",
		CommandTruncated: os.Getenv("PLY_COMMAND_TRUNCATED") == "1",
	})
}
func approvalEnvironment() []string {
	overrides := map[string]string{}
	if model := os.Getenv("PLY_APPROVE_MODEL"); model != "" {
		overrides["PLY_MODEL"] = model
	}
	if window := os.Getenv("PLY_APPROVE_CONTEXT_WINDOW"); window != "" && window != "0" {
		overrides["PLY_CONTEXT_WINDOW"] = window
	}
	result := []string{}
	for _, pair := range os.Environ() {
		key, _, _ := strings.Cut(pair, "=")
		if _, ok := overrides[key]; !ok {
			result = append(result, pair)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}
func parseDecision(s string) (string, string, error) {
	decoder := json.NewDecoder(strings.NewReader(s))
	token, e := decoder.Token()
	if e != nil || token != json.Delim('{') {
		return "", "", fmt.Errorf("expected a JSON object")
	}
	fields := map[string]string{}
	for decoder.More() {
		token, e = decoder.Token()
		if e != nil {
			return "", "", e
		}
		key, ok := token.(string)
		if !ok || (key != "decision" && key != "reason") {
			return "", "", fmt.Errorf("unexpected field")
		}
		if _, exists := fields[key]; exists {
			return "", "", fmt.Errorf("duplicate field %s", key)
		}
		var value string
		token, e = decoder.Token()
		if e != nil {
			return "", "", e
		}
		value, ok = token.(string)
		if !ok {
			return "", "", fmt.Errorf("%s must be a string", key)
		}
		fields[key] = value
	}
	if _, e = decoder.Token(); e != nil {
		return "", "", e
	}
	if e = decoder.Decode(new(any)); e != io.EOF {
		return "", "", fmt.Errorf("expected one JSON object")
	}
	switch fields["decision"] {
	case "APPROVE", "DENY", "UNSURE":
	default:
		return "", "", fmt.Errorf("unknown decision")
	}
	reason := strings.TrimSpace(fields["reason"])
	if reason == "" {
		return "", "", fmt.Errorf("reason is required")
	}
	return fields["decision"], reason, nil
}
