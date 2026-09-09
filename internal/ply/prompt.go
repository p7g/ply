package ply

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const basePrompt = `You are ply, a minimal coding agent. Complete the user's request carefully and verify your work.
You have two tools: bash and plan. Use shell commands to inspect, edit, search, and test files.
Each bash call starts a fresh shell in the transcript's working directory. cd, export, and functions do not persist. stdin is /dev/null. Commands require approval; give a clear justification.
Long-running commands (builds, test suites, servers) should run in the background. If you start a background task and have nothing else useful to do, end your turn — you will be woken with the result when it finishes.
The plan tool replaces the current plan. Keep it useful and current.
Subagents are ordinary background commands: ply --subagent -m 'task' sub-ID.jsonl. Their approvals are forwarded to the parent. Use unique transcript paths.
`

func systemPrompt(ctx context.Context, cwd string, c Config) (string, []string, error) {
	parts := []string{basePrompt}
	sources := []string{"builtin"}
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, _ := os.ReadDir(dir)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, "ply-") || strings.HasPrefix(name, "ply-approve-") || seen[name] {
				continue
			}
			path := filepath.Join(dir, name)
			info, e := os.Stat(path)
			if e != nil || info.IsDir() || info.Mode()&0111 == 0 {
				continue
			}
			seen[name] = true
			child, cancel := context.WithTimeout(ctx, 2*time.Second)
			cmd := exec.CommandContext(child, path, "--ply-prompt")
			cmd.Dir = cwd
			b, e := cmd.Output()
			cancel()
			if e == nil && strings.TrimSpace(string(b)) != "" {
				parts = append(parts, string(b))
				sources = append(sources, path)
			}
		}
	}
	dirs := ancestors(cwd)
	for n := len(dirs) - 1; n >= 0; n-- {
		p := filepath.Join(dirs[n], "AGENTS.md")
		b, e := os.ReadFile(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return "", nil, e
		}
		parts = append(parts, string(b))
		sources = append(sources, p)
	}
	if p := c.S("system_file"); p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return "", nil, fmt.Errorf("system_file: %w", e)
		}
		parts = append(parts, string(b))
		sources = append(sources, p)
	}
	return strings.Join(parts, "\n\n"), sources, nil
}
